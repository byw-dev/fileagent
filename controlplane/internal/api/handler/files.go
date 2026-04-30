package handler

import (
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/gin-gonic/gin"
)

// FilesHandler groups the file-entry management handlers.
type FilesHandler struct{}

// NewFilesHandler returns a new FilesHandler.
func NewFilesHandler() *FilesHandler { return &FilesHandler{} }

// List handles GET /api/v1/files.
func (h *FilesHandler) List(c *gin.Context) { middleware.NotImplemented(c) }

// Get handles GET /api/v1/files/:id.
func (h *FilesHandler) Get(c *gin.Context) { middleware.NotImplemented(c) }

// DownloadURL handles GET /api/v1/files/:id/download-url.
func (h *FilesHandler) DownloadURL(c *gin.Context) { middleware.NotImplemented(c) }

// BatchDownloadURLs handles POST /api/v1/files/batch-download-urls.
func (h *FilesHandler) BatchDownloadURLs(c *gin.Context) { middleware.NotImplemented(c) }

// FileTypesHandler groups the file-type management handlers.
type FileTypesHandler struct{}

// NewFileTypesHandler returns a new FileTypesHandler.
func NewFileTypesHandler() *FileTypesHandler { return &FileTypesHandler{} }

// List handles GET /api/v1/file-types.
func (h *FileTypesHandler) List(c *gin.Context) { middleware.NotImplemented(c) }

// Create handles POST /api/v1/file-types.
func (h *FileTypesHandler) Create(c *gin.Context) { middleware.NotImplemented(c) }

// Update handles PUT /api/v1/file-types/:id.
func (h *FileTypesHandler) Update(c *gin.Context) { middleware.NotImplemented(c) }

// Delete handles DELETE /api/v1/file-types/:id.
func (h *FileTypesHandler) Delete(c *gin.Context) { middleware.NotImplemented(c) }
