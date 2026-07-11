package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	tagSourceManual      = "manual"
	tagAuditActionSet    = "set"
	tagAuditActionClear  = "clear"
	maxManualTagValueLen = 128
)

// FileTagsDB is the minimal database interface for manual single-file tagging.
// It is satisfied by *db.Queries.
type FileTagsDB interface {
	GetFileEntryByID(ctx context.Context, id uuid.UUID) (*db.FileEntry, error)
	GetTagKey(ctx context.Context, orgID uuid.UUID, key string) (*db.TagKey, error)
	GetFileTagValue(ctx context.Context, fileEntryID uuid.UUID, key string) (string, error)
	SetFileTag(ctx context.Context, fileEntryID uuid.UUID, key string, value string, source string) error
	DeleteFileTag(ctx context.Context, fileEntryID uuid.UUID, key string) (int64, error)
	TagValueExists(ctx context.Context, tagKeyID uuid.UUID, value string) (bool, error)
	FindSimilarTagValue(ctx context.Context, tagKeyID uuid.UUID, lower string) (string, error)
	UpsertPendingTagValueManual(ctx context.Context, arg db.UpsertPendingTagValueManualParams) error
	CreateTagAudit(ctx context.Context, arg db.CreateTagAuditParams) error
	ListFileTagsByFileIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]map[string]string, error)
}

// FileTagsHandler serves manual single-file tagging (metadata 6c, Phase 1). It is
// mounted under /files and gated to super_admin at the router.
type FileTagsHandler struct {
	db     FileTagsDB
	logger *zap.Logger
}

// NewFileTagsHandler returns a new handler. A nil db causes handlers to return
// 501 until dependencies are wired.
func NewFileTagsHandler(fileTagsDB FileTagsDB, logger *zap.Logger) *FileTagsHandler {
	return &FileTagsHandler{db: fileTagsDB, logger: logger}
}

// setTagsRequest is the body for PUT /api/v1/files/:id/tags. A key mapped to a
// JSON string sets that tag; a key mapped to null clears it. A key absent from
// the map is left untouched.
type setTagsRequest struct {
	Tags map[string]*string `json:"tags"`
}

// SetTags handles PUT /api/v1/files/:id/tags. It applies a partial set/clear of
// a single file's tags with source=manual, writing a tag_audit row per change.
// Unregistered values of a controlled key are still recorded but queued in
// pending_tag_values (governance parity with path-variable extraction).
//
// Keys are validated up front (format + registration for sets) so the apply loop
// only fails on infrastructure errors; the applies are sequential, not a single
// transaction.
func (h *FileTagsHandler) SetTags(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid file id", nil)
		return
	}

	entry, err := h.db.GetFileEntryByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "file not found", nil)
			return
		}
		h.logger.Error("get file for tagging", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to get file", nil)
		return
	}
	orgID := orgIDFromClaims(c)
	if entry.OrgID != orgID {
		// Do not leak existence across tenants.
		middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "file not found", nil)
		return
	}

	var req setTagsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}
	if len(req.Tags) == 0 {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "tags must not be empty", nil)
		return
	}

	// Validate up front: every set-key must be a registered tag key with an
	// in-range value; clears are unconstrained (a tag with an unregistered key
	// may exist from path extraction and must remain clearable).
	setKeys := make(map[string]*db.TagKey)
	for key, val := range req.Tags {
		if !isValidTagKey(key) {
			middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid tag key: "+key, nil)
			return
		}
		if val == nil {
			continue // clear
		}
		value := *val
		if strings.TrimSpace(value) == "" {
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
			h.logger.Error("get tag key", zap.Error(err))
			middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to validate tag key", nil)
			return
		}
		setKeys[key] = tagKey
	}

	actor := actorNullUUID(c)
	var setDone, clearedDone []string
	for key, val := range req.Tags {
		if val == nil {
			cleared, err := h.clearTag(c, orgID, entry.ID, key, actor)
			if err != nil {
				middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to clear tag", nil)
				return
			}
			if cleared {
				clearedDone = append(clearedDone, key)
			}
			continue
		}
		if err := h.setTag(c, orgID, entry.ID, setKeys[key], key, *val, actor); err != nil {
			middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to set tag", nil)
			return
		}
		setDone = append(setDone, key)
	}

	tagsByFile, err := h.db.ListFileTagsByFileIDs(c.Request.Context(), []uuid.UUID{entry.ID})
	if err != nil {
		h.logger.Error("list file tags after set", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load tags", nil)
		return
	}
	sort.Strings(setDone)
	sort.Strings(clearedDone)
	c.JSON(http.StatusOK, gin.H{
		"set":     setDone,
		"cleared": clearedDone,
		"tags":    tagsByFile[entry.ID],
	})
}

// setTag writes/overwrites one manual tag and its audit row, queuing an
// unregistered controlled value for review.
func (h *FileTagsHandler) setTag(c *gin.Context, orgID, fileEntryID uuid.UUID, tagKey *db.TagKey, key, value string, actor uuid.NullUUID) error {
	old, hadOld, err := h.oldValue(c, fileEntryID, key)
	if err != nil {
		return err
	}
	if err := h.db.SetFileTag(c.Request.Context(), fileEntryID, key, value, tagSourceManual); err != nil {
		return err
	}
	if err := h.db.CreateTagAudit(c.Request.Context(), db.CreateTagAuditParams{
		OrgID:       orgID,
		FileEntryID: uuid.NullUUID{UUID: fileEntryID, Valid: true},
		Key:         key,
		OldValue:    sql.NullString{String: old, Valid: hadOld},
		NewValue:    sql.NullString{String: value, Valid: true},
		Action:      tagAuditActionSet,
		ActorUserID: actor,
		Source:      tagSourceManual,
	}); err != nil {
		return err
	}
	return h.queuePendingIfUnregistered(c, orgID, tagKey, value)
}

// clearTag removes one manual tag (if present) and writes its audit row.
func (h *FileTagsHandler) clearTag(c *gin.Context, orgID, fileEntryID uuid.UUID, key string, actor uuid.NullUUID) (bool, error) {
	old, hadOld, err := h.oldValue(c, fileEntryID, key)
	if err != nil {
		return false, err
	}
	rows, err := h.db.DeleteFileTag(c.Request.Context(), fileEntryID, key)
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, nil // nothing to clear; no audit
	}
	err = h.db.CreateTagAudit(c.Request.Context(), db.CreateTagAuditParams{
		OrgID:       orgID,
		FileEntryID: uuid.NullUUID{UUID: fileEntryID, Valid: true},
		Key:         key,
		OldValue:    sql.NullString{String: old, Valid: hadOld},
		Action:      tagAuditActionClear,
		ActorUserID: actor,
		Source:      tagSourceManual,
	})
	return err == nil, err
}

// oldValue returns the current value of a file tag (hadOld=false when absent).
func (h *FileTagsHandler) oldValue(c *gin.Context, fileEntryID uuid.UUID, key string) (string, bool, error) {
	v, err := h.db.GetFileTagValue(c.Request.Context(), fileEntryID, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

// queuePendingIfUnregistered queues a controlled key's unregistered value for
// admin review (governance parity with path-variable extraction).
func (h *FileTagsHandler) queuePendingIfUnregistered(c *gin.Context, orgID uuid.UUID, tagKey *db.TagKey, value string) error {
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

// actorNullUUID returns the caller's user id for audit attribution, or a null
// UUID when it is unavailable.
func actorNullUUID(c *gin.Context) uuid.NullUUID {
	if id := callerIDFromClaims(c); id != uuid.Nil {
		return uuid.NullUUID{UUID: id, Valid: true}
	}
	return uuid.NullUUID{}
}
