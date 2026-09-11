package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
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
	CountFileEntries(ctx context.Context, f db.CountFileEntriesFilter) (int64, error)
	GetFileEntryByID(ctx context.Context, id uuid.UUID) (*db.FileEntry, error)
	// GetBucketByID is used to resolve a bucket UUID to its MinIO bucket name.
	GetBucketByID(ctx context.Context, id uuid.UUID) (*db.Bucket, error)
	// ListFileTagsByFileIDs returns the tags of each file, keyed by file id then
	// tag key. Used to attach tags to file responses.
	ListFileTagsByFileIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]map[string]string, error)
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

// fetchOwnedEntry loads a file entry and confirms it belongs to the caller's
// org (GetFileEntryByID selects by id only). It writes the response and returns
// ok=false when the entry is missing, unreadable, or owned by another org.
// Cross-org access is reported as 404 (not 403) so a caller cannot probe another
// tenant's file IDs.
func (h *FilesHandler) fetchOwnedEntry(c *gin.Context, id uuid.UUID) (*db.FileEntry, bool) {
	entry, err := h.db.GetFileEntryByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "file not found", nil)
			return nil, false
		}
		h.logger.Error("get file entry", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to get file", nil)
		return nil, false
	}
	if entry.OrgID != orgIDFromClaims(c) {
		middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "file not found", nil)
		return nil, false
	}
	return entry, true
}

// fileEntryResponse is the outbound JSON shape for a file entry.
type fileEntryResponse struct {
	MetaIncomplete bool              `json:"meta_incomplete"`
	ID             string            `json:"id"`
	OrgID          string            `json:"org_id"`
	AgentID        string            `json:"agent_id,omitempty"`
	BucketID       string            `json:"bucket_id"`
	FileTypeID     string            `json:"file_type_id,omitempty"`
	StorageKey     string            `json:"storage_key"`
	OriginalPath   string            `json:"original_path,omitempty"`
	Filename       string            `json:"filename"`
	Size           int64             `json:"size"`
	SHA256         string            `json:"sha256,omitempty"`
	MimeType       string            `json:"mime_type,omitempty"`
	Status         string            `json:"status"`
	UploadedAt     string            `json:"uploaded_at,omitempty"`
	CreatedAt      string            `json:"created_at"`
	Tags           map[string]string `json:"tags,omitempty"`
}

func toFileEntryResponse(e *db.FileEntry, tags map[string]string) fileEntryResponse {
	r := fileEntryResponse{
		MetaIncomplete: e.MetaIncomplete,
		ID:             e.ID.String(),
		OrgID:          e.OrgID.String(),
		BucketID:       e.BucketID.String(),
		StorageKey:     e.StoragePath,
		Filename:       e.FileName,
		Size:           e.SizeBytes,
		Status:         strings.ToUpper(string(e.Status)),
		CreatedAt:      e.CreatedAt.UTC().Format(time.RFC3339),
		Tags:           tags,
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
		r.MimeType = e.ContentType.String
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
	// Reject mistyped/unsupported filters instead of silently ignoring them,
	// which would return 200 with the filter having no effect (CC-5).
	if !middleware.RejectUnknownQuery(c, "cursor", "limit", "agent_id", "bucket_id", "file_type_id", "status", "tag") {
		return
	}
	orgID := orgIDFromClaims(c)
	limit := parseLimitParam(c)

	cursorCreatedAt, cursorID, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_CURSOR", "invalid cursor", nil)
		return
	}

	// Parse repeatable tag predicates (?tag=key:value), combined with AND.
	tags, ok := parseTagFilters(c)
	if !ok {
		return
	}

	// Collect optional filters for both list and count.
	filter := db.CountFileEntriesFilter{OrgID: orgID, Tags: tags}
	params := db.ListFileEntriesParams{
		OrgID:           orgID,
		Tags:            tags,
		CursorCreatedAt: cursorCreatedAt,
		CursorID:        cursorID,
		Limit:           limit + 1, // fetch one extra to detect has_more
	}

	if v := c.Query("agent_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			nid := uuid.NullUUID{UUID: id, Valid: true}
			params.AgentID = nid
			filter.AgentID = nid
		}
	}
	if v := c.Query("bucket_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			nid := uuid.NullUUID{UUID: id, Valid: true}
			params.BucketID = nid
			filter.BucketID = nid
		}
	}
	if v := c.Query("file_type_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			nid := uuid.NullUUID{UUID: id, Valid: true}
			params.FileTypeID = nid
			filter.FileTypeID = nid
		}
	}
	if v := c.Query("status"); v != "" {
		s := db.FileStatus(v)
		ns := db.NullFileStatus{FileStatus: s, Valid: true}
		params.Status = ns
		filter.Status = ns
	}

	entries, err := h.db.ListFileEntries(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("list file entries", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list files", nil)
		return
	}

	hasMore := len(entries) > int(limit)
	if hasMore {
		entries = entries[:limit]
	}

	total, err := h.db.CountFileEntries(c.Request.Context(), filter)
	if err != nil {
		h.logger.Error("count file entries", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to count files", nil)
		return
	}

	// Batch-fetch tags for the page so responses carry their tags without an
	// N+1 query per file.
	ids := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	tagsByFile, err := h.db.ListFileTagsByFileIDs(c.Request.Context(), ids)
	if err != nil {
		h.logger.Error("list file tags", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load tags", nil)
		return
	}

	resp := make([]fileEntryResponse, 0, len(entries))
	for _, e := range entries {
		resp = append(resp, toFileEntryResponse(e, tagsByFile[e.ID]))
	}

	var nextCursor string
	if hasMore {
		last := entries[len(entries)-1]
		nextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	c.JSON(http.StatusOK, gin.H{
		"items":       resp,
		"total":       total,
		"has_more":    hasMore,
		"next_cursor": nextCursor,
	})
}

// Get handles GET /api/v1/files/:id.
func (h *FilesHandler) Get(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid file id", nil)
		return
	}
	entry, ok := h.fetchOwnedEntry(c, id)
	if !ok {
		return
	}
	tagsByFile, err := h.db.ListFileTagsByFileIDs(c.Request.Context(), []uuid.UUID{entry.ID})
	if err != nil {
		h.logger.Error("get file tags", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load tags", nil)
		return
	}
	c.JSON(http.StatusOK, toFileEntryResponse(entry, tagsByFile[entry.ID]))
}

// maxTagFilters bounds the number of distinct tag predicates a single files
// query may carry, guarding against oversized generated SQL.
const maxTagFilters = 20

// parseTagFilters parses repeatable ?tag=key:value query parameters into tag
// predicates, combined with AND. It writes a 400 and returns ok=false on a
// malformed value (empty key, or missing ':'), turning a silent no-op filter
// into an explicit client error (consistent with RejectUnknownQuery / CC-5).
//
// Because file_tags is keyed on (file_entry_id, key), a file carries at most one
// value per key, so the AND filter (HAVING COUNT(*) = N) only makes sense with
// distinct keys. Exact duplicate predicates are collapsed; two different values
// for the same key can never both match, so they are rejected with a 400 rather
// than silently returning zero results.
func parseTagFilters(c *gin.Context) ([]db.FileTagFilter, bool) {
	return parseTagPredicates(c, c.QueryArray("tag"))
}

// parseTagPredicates parses key:value tag predicates (from a query array or a
// request body) into AND-combined filters, applying the same validation as
// parseTagFilters (format, distinct-key, count bound). It is shared by GET /files
// and the batch-tag selection filter.
func parseTagPredicates(c *gin.Context, raw []string) ([]db.FileTagFilter, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	filters := make([]db.FileTagFilter, 0, len(raw))
	seen := make(map[string]string, len(raw))
	for _, t := range raw {
		key, value, found := strings.Cut(t, ":")
		if !found || key == "" || value == "" {
			middleware.RespondError(c, http.StatusBadRequest, "INVALID_QUERY_PARAM",
				"tag must be formatted as key:value", gin.H{"tag": t})
			return nil, false
		}
		// Bound the number of distinct predicates: each adds two bind params and
		// expands the generated IN-list, so an unbounded request would produce
		// very large SQL and excessive DB work (availability guard).
		if _, ok := seen[key]; !ok && len(filters) >= maxTagFilters {
			middleware.RespondError(c, http.StatusBadRequest, "INVALID_QUERY_PARAM",
				"too many tag filters (max "+strconv.Itoa(maxTagFilters)+")", nil)
			return nil, false
		}
		if prev, ok := seen[key]; ok {
			if prev != value {
				middleware.RespondError(c, http.StatusBadRequest, "INVALID_QUERY_PARAM",
					"conflicting values for tag key: "+key, gin.H{"key": key})
				return nil, false
			}
			continue // exact duplicate — already included
		}
		seen[key] = value
		filters = append(filters, db.FileTagFilter{Key: key, Value: value})
	}
	return filters, true
}

// DownloadURL handles GET /api/v1/files/:id/download-url.
func (h *FilesHandler) DownloadURL(c *gin.Context) {
	if h.db == nil || h.minio == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid file id", nil)
		return
	}
	entry, ok := h.fetchOwnedEntry(c, id)
	if !ok {
		return
	}

	bucket, err := h.db.GetBucketByID(c.Request.Context(), entry.BucketID)
	if err != nil {
		h.logger.Error("get bucket for download", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to resolve bucket", nil)
		return
	}

	const presignTTL = 15 * time.Minute
	url, err := h.minio.PresignedGetObject(c.Request.Context(), bucket.Name, entry.StoragePath, presignTTL)
	if err != nil {
		h.logger.Error("presign url", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to generate download URL", nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url, "expires_in": int(presignTTL.Seconds())})
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
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
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
		// Do not hand out a presigned URL for another org's object; report it as
		// not found rather than leaking existence across tenants.
		if entry.OrgID != orgIDFromClaims(c) {
			result = append(result, urlItem{ID: rawID, Err: "not found"})
			continue
		}
		bucket, err := h.db.GetBucketByID(c.Request.Context(), entry.BucketID)
		if err != nil {
			h.logger.Error("get bucket for batch download", zap.String("id", rawID), zap.Error(err))
			result = append(result, urlItem{ID: rawID, Err: "bucket not found"})
			continue
		}
		const presignTTL = 15 * time.Minute
		url, err := h.minio.PresignedGetObject(c.Request.Context(), bucket.Name, entry.StoragePath, presignTTL)
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
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list file types", nil)
		return
	}
	resp := make([]fileTypeResponse, 0, len(types))
	for _, ft := range types {
		resp = append(resp, toFileTypeResponse(ft))
	}
	c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp)})
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
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
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
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create file type", nil)
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
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid file type id", nil)
		return
	}

	var req updateFileTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	var desc sql.NullString
	if req.Description != "" {
		desc = sql.NullString{String: req.Description, Valid: true}
	}

	ft, err := h.db.UpdateFileType(c.Request.Context(), id, req.Name, desc)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "file type not found", nil)
			return
		}
		h.logger.Error("update file type", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update file type", nil)
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
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid file type id", nil)
		return
	}
	if err := h.db.DeleteFileType(c.Request.Context(), id); err != nil {
		h.logger.Error("delete file type", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete file type", nil)
		return
	}
	c.Status(http.StatusNoContent)
}
