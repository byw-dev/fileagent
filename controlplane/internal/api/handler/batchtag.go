package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/retag"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// BatchTagDB is the minimal database interface needed by BatchTagHandler. It is
// satisfied by *db.Queries.
type BatchTagDB interface {
	GetTagKey(ctx context.Context, orgID uuid.UUID, key string) (*db.TagKey, error)
	TagValueExists(ctx context.Context, tagKeyID uuid.UUID, value string) (bool, error)
	FindSimilarTagValue(ctx context.Context, tagKeyID uuid.UUID, lower string) (string, error)
	UpsertPendingTagValueManual(ctx context.Context, arg db.UpsertPendingTagValueManualParams) error
	EnqueueRetagJob(ctx context.Context, orgID uuid.UUID, kind string, spec json.RawMessage, actorUserID uuid.NullUUID) (*db.RetagJob, error)
}

// BatchTagHandler serves bulk tagging: apply a set/clear of tags to every file
// matching a GET /files-style predicate (metadata 6c, Phase 1 — MT-5b). It serves
// historical-data backfill. The apply runs asynchronously on worker.RetagWorker;
// the endpoint validates + enqueues and returns 202. Mounted under /files and
// gated to super_admin at the router.
type BatchTagHandler struct {
	db     BatchTagDB
	logger *zap.Logger
}

// NewBatchTagHandler returns a new handler. A nil db causes handlers to return
// 501 until dependencies are wired.
func NewBatchTagHandler(batchDB BatchTagDB, logger *zap.Logger) *BatchTagHandler {
	return &BatchTagHandler{db: batchDB, logger: logger}
}

// batchTagRequest is the body for POST /api/v1/files/batch-tag. `filter` selects
// the files (same predicate as GET /files); `tags` maps a key to a string (set)
// or null (clear), applied to every matching file.
type batchTagRequest struct {
	Filter batchTagFilterReq  `json:"filter"`
	Tags   map[string]*string `json:"tags"`
}

type batchTagFilterReq struct {
	AgentID    string   `json:"agent_id"`
	BucketID   string   `json:"bucket_id"`
	FileTypeID string   `json:"file_type_id"`
	Status     string   `json:"status"`
	Tags       []string `json:"tags"` // repeatable key:value predicates (AND)
}

// Submit handles POST /api/v1/files/batch-tag. It validates the tag operations
// (registered keys for sets, value bounds; clears unconstrained) and selection
// filter up front, queues unregistered controlled values for review (governance
// parity with single-file tagging), then enqueues a batch_tag retag job.
func (h *BatchTagHandler) Submit(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)

	var req batchTagRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}
	if len(req.Tags) == 0 {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "tags must not be empty", nil)
		return
	}

	// Validate the selection filter (reuses GET /files predicate parsing + bounds).
	filterSpec, ok := h.parseFilter(c, req.Filter)
	if !ok {
		return
	}

	// Validate tag ops up front; collect set values (trimmed) so the persisted
	// value matches the controlled vocabulary path. Also gather controlled keys to
	// queue their unregistered values after validation succeeds.
	normalized := make(map[string]*string, len(req.Tags))
	type setOp struct {
		tagKey *db.TagKey
		value  string
	}
	var setOps []setOp
	for key, val := range req.Tags {
		if !isValidTagKey(key) {
			middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid tag key: "+key, nil)
			return
		}
		if val == nil {
			normalized[key] = nil // clear
			continue
		}
		value := strings.TrimSpace(*val)
		if value == "" {
			middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "value must not be empty for key: "+key, nil)
			return
		}
		if utf8.RuneCountInString(value) > maxManualTagValueLen {
			middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "value too long for key: "+key, nil)
			return
		}
		tagKey, err := h.db.GetTagKey(c.Request.Context(), orgID, key)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				middleware.RespondError(c, http.StatusBadRequest, "UNKNOWN_TAG_KEY", "tag key not registered: "+key, nil)
				return
			}
			h.logger.Error("batch-tag: get tag key", zap.Error(err))
			middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to validate tag key", nil)
			return
		}
		v := value
		normalized[key] = &v
		setOps = append(setOps, setOp{tagKey: tagKey, value: value})
	}

	// Governance parity with single-file tagging: a controlled key's unregistered
	// value is queued for review (once per key — the value is identical across all
	// matched files). Files still get the value; it just is not a filter option yet.
	for _, op := range setOps {
		if err := h.queuePendingIfUnregistered(c, orgID, op.tagKey, op.value); err != nil {
			h.logger.Error("batch-tag: queue pending value", zap.Error(err))
			middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to queue value for review", nil)
			return
		}
	}

	spec, err := json.Marshal(retag.BatchTagSpec{Filter: filterSpec, Tags: normalized})
	if err != nil { // unreachable for a serialisable struct, but stay explicit
		h.logger.Error("batch-tag: marshal spec", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to enqueue batch tag", nil)
		return
	}
	job, err := h.db.EnqueueRetagJob(c.Request.Context(), orgID, retag.KindBatchTag, spec, actorNullUUID(c))
	if err != nil {
		h.logger.Error("batch-tag: enqueue job", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to enqueue batch tag", nil)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID.String(), "status": job.Status})
}

// parseFilter validates the selection filter and converts it to the spec form.
// UUID fields must parse when present; tag predicates reuse the GET /files parser.
func (h *BatchTagHandler) parseFilter(c *gin.Context, f batchTagFilterReq) (retag.BatchTagFilter, bool) {
	out := retag.BatchTagFilter{Status: f.Status}
	// Reject an unknown status up front: it would otherwise enqueue a job that
	// only fails asynchronously when cast to the file_status enum in SQL.
	if f.Status != "" && !db.FileStatus(f.Status).Valid() {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid status", nil)
		return out, false
	}
	for _, field := range []struct {
		name, val string
		dst       *string
	}{
		{"agent_id", f.AgentID, &out.AgentID},
		{"bucket_id", f.BucketID, &out.BucketID},
		{"file_type_id", f.FileTypeID, &out.FileTypeID},
	} {
		if field.val == "" {
			continue
		}
		if _, err := uuid.Parse(field.val); err != nil {
			middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid "+field.name, nil)
			return out, false
		}
		*field.dst = field.val
	}
	preds, ok := parseTagPredicates(c, f.Tags)
	if !ok {
		return out, false
	}
	for _, p := range preds {
		out.Tags = append(out.Tags, retag.TagPredicate{Key: p.Key, Value: p.Value})
	}
	return out, true
}

// queuePendingIfUnregistered queues a controlled key's unregistered value for
// admin review (governance parity with single-file tagging / path extraction).
func (h *BatchTagHandler) queuePendingIfUnregistered(c *gin.Context, orgID uuid.UUID, tagKey *db.TagKey, value string) error {
	if !tagKey.ValueControlled {
		return nil
	}
	exists, err := h.db.TagValueExists(c.Request.Context(), tagKey.ID, value)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	suggested, err := h.db.FindSimilarTagValue(c.Request.Context(), tagKey.ID, value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return h.db.UpsertPendingTagValueManual(c.Request.Context(), db.UpsertPendingTagValueManualParams{
		OrgID:          orgID,
		TagKeyID:       tagKey.ID,
		ExtractedValue: value,
		Source:         tagSourceManual,
		SuggestedValue: sql.NullString{String: suggested, Valid: suggested != ""},
	})
}
