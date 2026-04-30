package handler

import (
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/gin-gonic/gin"
)

// BucketsHandler groups the Bucket management handlers.
type BucketsHandler struct{}

// NewBucketsHandler returns a new BucketsHandler.
func NewBucketsHandler() *BucketsHandler { return &BucketsHandler{} }

// List handles GET /api/v1/buckets.
func (h *BucketsHandler) List(c *gin.Context) { middleware.NotImplemented(c) }

// Create handles POST /api/v1/buckets.
func (h *BucketsHandler) Create(c *gin.Context) { middleware.NotImplemented(c) }

// EventRulesHandler groups the event-rule management handlers.
type EventRulesHandler struct{}

// NewEventRulesHandler returns a new EventRulesHandler.
func NewEventRulesHandler() *EventRulesHandler { return &EventRulesHandler{} }

// List handles GET /api/v1/event-rules.
func (h *EventRulesHandler) List(c *gin.Context) { middleware.NotImplemented(c) }

// Create handles POST /api/v1/event-rules.
func (h *EventRulesHandler) Create(c *gin.Context) { middleware.NotImplemented(c) }

// Update handles PUT /api/v1/event-rules/:id.
func (h *EventRulesHandler) Update(c *gin.Context) { middleware.NotImplemented(c) }

// Delete handles DELETE /api/v1/event-rules/:id.
func (h *EventRulesHandler) Delete(c *gin.Context) { middleware.NotImplemented(c) }

// ListDeliveries handles GET /api/v1/event-rules/:id/deliveries.
func (h *EventRulesHandler) ListDeliveries(c *gin.Context) { middleware.NotImplemented(c) }

// UploadLogsHandler groups the upload-log query handlers.
type UploadLogsHandler struct{}

// NewUploadLogsHandler returns a new UploadLogsHandler.
func NewUploadLogsHandler() *UploadLogsHandler { return &UploadLogsHandler{} }

// List handles GET /api/v1/upload-logs.
func (h *UploadLogsHandler) List(c *gin.Context) { middleware.NotImplemented(c) }

// Get handles GET /api/v1/upload-logs/:id.
func (h *UploadLogsHandler) Get(c *gin.Context) { middleware.NotImplemented(c) }
