package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// FilesDB is the minimal database interface needed by FilesHandler.
type FilesDB interface {
	ListFileEntries(ctx context.Context, arg db.ListFileEntriesParams) ([]*db.FileEntry, error)
	GetFileEntryByID(ctx context.Context, id uuid.UUID) (*db.FileEntry, error)
}

// MinIOPresigner generates presigned download URLs for stored objects.
type MinIOPresigner interface {
	PresignedGetObject(ctx context.Context, bucketName, objectName string, expiry time.Duration) (string, error)
}

// FilesHandler groups the file-entry query and download handlers.
type FilesHandler struct {
	db     FilesDB
	minio  MinIOPresigner
	logger *zap.Logger
}

// NewFilesHandler returns a new FilesHandler. Nil arguments cause handlers to
// return 501 until dependencies are wired.
func NewFilesHandler(filesDB FilesDB, minio MinIOPresigner, logger *zap.Logger) *FilesHandler {
	return &FilesHandler{db: filesDB, minio: minio, logger: logger}
}

// fileEntryResponse is the outbound JSON shape for a file entry.
type fileEntryResponse struct {
	ID           string `json:"id"`
	OrgID        string `json:"org_id"`
	AgentID      string `json:"agent_id,omitempty"`
	BucketID     string `json:"bucket_id"`
	FileTypeID   string `json:"file_type_id,omitempty"`
	StoragePath  string `json:"storage_path"`
	OriginalPath string `json:"original_path,omitempty"`
	FileName     string `json:"file_name"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	Status       string `json:"status"`
	UploadedAt   string `json:"uploaded_at,omitempty"`
	CreatedAt    string `json:"created_at"`
}

func toFileEntryResponse(e *db.FileEntry) fileEntryResponse {
	r := fileEntryResponse{
		ID:          e.ID.String(),
		OrgID:       e.OrgID.String(),
		BucketID:    e.BucketID.String(),
		StoragePath: e.StoragePath,
		FileName:    e.FileName,
		SizeBytes:   e.SizeBytes,
		Status:      string(e.Status),
		CreatedAt:   e.CreatedAt.UTC().Format(time.RFC3339),
	}
	if e.AgentID.Valid {
		r.AgentID = e.AgentID.UUID.String()
	}
	if e.FileTypeID.Valid {
		r.FileTypeID = e.FileTypeID.UUID.String()
	}
	if e.OriginalPath.Valid {
		r.OriginalPath = e.OriginalPath.String
	}
	if e.Sha256.Valid {
		r.SHA256 = e.Sha256.String
	}
	if e.ContentType.Valid {
		r.ContentType = e.ContentType.String
	}
	if e.UploadedAt.Valid {
		r.UploadedAt = e.UploadedAt.Time.UTC().Format(time.RFC3339)
	}
	return r
}

// List handles GET /api/v1/files.
func (h *FilesHandler) List(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	limit := parseLimitParam(c)

	cursorCreatedAt, cursorID, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_CURSOR", "invalid cursor", nil),
		})
		return
	}

	params := db.ListFileEntriesParams{
		OrgID:           orgID,
		CursorCreatedAt: cursorCreatedAt,
		CursorID:        cursorID,
		Limit:           limit,
	}

	// Optional filters.
	if v := c.Query("agent_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			params.AgentID = uuid.NullUUID{UUID: id, Valid: true}
		}
	}
	if v := c.Query("bucket_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			params.BucketID = uuid.NullUUID{UUID: id, Valid: true}
		}
	}
	if v := c.Query("file_type_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			params.FileTypeID = uuid.NullUUID{UUID: id, Valid: true}
		}
	}
	if v := c.Query("status"); v != "" {
		s := db.FileStatus(v)
		params.Status = db.NullFileStatus{FileStatus: s, Valid: true}
	}

	entries, err := h.db.ListFileEntries(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("list file entries", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to list files", nil),
		})
		return
	}

	resp := make([]fileEntryResponse, 0, len(entries))
	for _, e := range entries {
		resp = append(resp, toFileEntryResponse(e))
	}

	var nextCursor string
	if len(entries) == int(limit) {
		last := entries[len(entries)-1]
		nextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	c.JSON(http.StatusOK, gin.H{"data": resp, "next_cursor": nextCursor})
}

// Get handles GET /api/v1/files/:id.
func (h *FilesHandler) Get(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid file id", nil),
		})
		return
	}
	entry, err := h.db.GetFileEntryByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": middleware.NewErrorBody("NOT_FOUND", "file not found", nil),
			})
			return
		}
		h.logger.Error("get file entry", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to get file", nil),
		})
		return
	}
	c.JSON(http.StatusOK, toFileEntryResponse(entry))
}

// DownloadURL handles GET /api/v1/files/:id/download-url.
func (h *FilesHandler) DownloadURL(c *gin.Context) {
	if h.db == nil || h.minio == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid file id", nil),
		})
		return
	}
	entry, err := h.db.GetFileEntryByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": middleware.NewErrorBody("NOT_FOUND", "file not found", nil),
			})
			return
		}
		h.logger.Error("get file for download", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to get file", nil),
		})
		return
	}

	url, err := h.minio.PresignedGetObject(c.Request.Context(), entry.BucketID.String(), entry.StoragePath, 5*time.Minute)
	if err != nil {
		h.logger.Error("presign url", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to generate download URL", nil),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url, "expires_in": 300})
}

// batchDownloadRequest is the body expected by POST /api/v1/files/batch-download-urls.
type batchDownloadRequest struct {
	IDs []string `json:"ids" binding:"required,min=1,max=100"`
}

// BatchDownloadURLs handles POST /api/v1/files/batch-download-urls.
func (h *FilesHandler) BatchDownloadURLs(c *gin.Context) {
	if h.db == nil || h.minio == nil {
		middleware.NotImplemented(c)
		return
	}

	var req batchDownloadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_REQUEST", err.Error(), nil),
		})
		return
	}

	type urlItem struct {
		ID  string `json:"id"`
		URL string `json:"url,omitempty"`
		Err string `json:"error,omitempty"`
	}
	result := make([]urlItem, 0, len(req.IDs))

	for _, rawID := range req.IDs {
		id, err := uuid.Parse(rawID)
		if err != nil {
			result = append(result, urlItem{ID: rawID, Err: "invalid id"})
			continue
		}
		entry, err := h.db.GetFileEntryByID(c.Request.Context(), id)
		if err != nil {
			result = append(result, urlItem{ID: rawID, Err: "not found"})
			continue
		}
		url, err := h.minio.PresignedGetObject(c.Request.Context(), entry.BucketID.String(), entry.StoragePath, 5*time.Minute)
		if err != nil {
			h.logger.Error("presign batch url", zap.String("id", rawID), zap.Error(err))
			result = append(result, urlItem{ID: rawID, Err: "presign failed"})
			continue
		}
		result = append(result, urlItem{ID: rawID, URL: url})
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

// ── FileTypesHandler ──────────────────────────────────────────────────────────

// FileTypesDB is the minimal database interface needed by FileTypesHandler.
type FileTypesDB interface {
	ListFileTypes(ctx context.Context, orgID uuid.UUID) ([]*db.FileType, error)
	GetFileTypeByID(ctx context.Context, id uuid.UUID) (*db.FileType, error)
	CreateFileType(ctx context.Context, orgID uuid.UUID, name string, description sql.NullString, createdBy uuid.NullUUID) (*db.FileType, error)
	UpdateFileType(ctx context.Context, iD uuid.UUID, name string, description sql.NullString) (*db.FileType, error)
	DeleteFileType(ctx context.Context, id uuid.UUID) error
}

// FileTypesHandler groups the file-type management handlers.
type FileTypesHandler struct {
	db     FileTypesDB
	logger *zap.Logger
}

// NewFileTypesHandler returns a new FileTypesHandler.
func NewFileTypesHandler(fileTypesDB FileTypesDB, logger *zap.Logger) *FileTypesHandler {
	return &FileTypesHandler{db: fileTypesDB, logger: logger}
}

// fileTypeResponse is the outbound JSON shape for a file type.
type fileTypeResponse struct {
	ID          string `json:"id"`
	OrgID       string `json:"org_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at"`
}

func toFileTypeResponse(ft *db.FileType) fileTypeResponse {
	r := fileTypeResponse{
		ID:        ft.ID.String(),
		OrgID:     ft.OrgID.String(),
		Name:      ft.Name,
		CreatedAt: ft.CreatedAt.UTC().Format(time.RFC3339),
	}
	if ft.Description.Valid {
		r.Description = ft.Description.String
	}
	return r
}

// List handles GET /api/v1/file-types.
func (h *FileTypesHandler) List(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	types, err := h.db.ListFileTypes(c.Request.Context(), orgID)
	if err != nil {
		h.logger.Error("list file types", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to list file types", nil),
		})
		return
	}
	resp := make([]fileTypeResponse, 0, len(types))
	for _, ft := range types {
		resp = append(resp, toFileTypeResponse(ft))
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

// createFileTypeRequest is the body expected by POST /api/v1/file-types.
type createFileTypeRequest struct {
	Name        string `json:"name"        binding:"required"`
	Description string `json:"description"`
}

// Create handles POST /api/v1/file-types.
func (h *FileTypesHandler) Create(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	callerID := callerIDFromClaims(c)

	var req createFileTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_REQUEST", err.Error(), nil),
		})
		return
	}

	var desc sql.NullString
	if req.Description != "" {
		desc = sql.NullString{String: req.Description, Valid: true}
	}
	var createdBy uuid.NullUUID
	if callerID != uuid.Nil {
		createdBy = uuid.NullUUID{UUID: callerID, Valid: true}
	}

	ft, err := h.db.CreateFileType(c.Request.Context(), orgID, req.Name, desc, createdBy)
	if err != nil {
		h.logger.Error("create file type", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to create file type", nil),
		})
		return
	}
	c.JSON(http.StatusCreated, toFileTypeResponse(ft))
}

// updateFileTypeRequest is the body expected by PUT /api/v1/file-types/:id.
type updateFileTypeRequest struct {
	Name        string `json:"name"        binding:"required"`
	Description string `json:"description"`
}

// Update handles PUT /api/v1/file-types/:id.
func (h *FileTypesHandler) Update(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid file type id", nil),
		})
		return
	}

	var req updateFileTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_REQUEST", err.Error(), nil),
		})
		return
	}

	var desc sql.NullString
	if req.Description != "" {
		desc = sql.NullString{String: req.Description, Valid: true}
	}

	ft, err := h.db.UpdateFileType(c.Request.Context(), id, req.Name, desc)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": middleware.NewErrorBody("NOT_FOUND", "file type not found", nil),
			})
			return
		}
		h.logger.Error("update file type", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to update file type", nil),
		})
		return
	}
	c.JSON(http.StatusOK, toFileTypeResponse(ft))
}

// Delete handles DELETE /api/v1/file-types/:id.
func (h *FileTypesHandler) Delete(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid file type id", nil),
		})
		return
	}
	if err := h.db.DeleteFileType(c.Request.Context(), id); err != nil {
		h.logger.Error("delete file type", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to delete file type", nil),
		})
		return
	}
	c.Status(http.StatusNoContent)
}
