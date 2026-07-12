// Package retag defines the contract between retag-job enqueuers (the REST API
// handlers) and the worker that drains them (worker.RetagWorker): the job kinds
// and the spec JSON shapes carried in retag_jobs.spec. Keeping it in one small
// package lets both sides share the exact shape without importing each other.
package retag

import "github.com/google/uuid"

// Job kinds stored in retag_jobs.kind. MT-5a implements KindMerge, MT-5b adds
// KindBatchTag; rule-retriggered retag would land later on the same channel.
const (
	KindMerge    = "merge"
	KindBatchTag = "batch_tag"
)

// MergeSpec is the spec for a KindMerge job: fold every occurrence of FromValue
// into the canonical ToValue for TagKeyID (org taken from the job), then drop the
// queued PendingID. ToValue is validated to be a registered value at enqueue.
type MergeSpec struct {
	TagKeyID  uuid.UUID `json:"tag_key_id"`
	FromValue string    `json:"from_value"`
	ToValue   string    `json:"to_value"`
	PendingID uuid.UUID `json:"pending_id"`
}

// BatchTagSpec is the spec for a KindBatchTag job: apply a set/clear of tags to
// every file matching Filter (the same predicate as GET /files). Tags maps a key
// to its new value (set) or to nil (clear); a set value's governance (registered
// key, pending queue for unregistered controlled values) is enforced at enqueue.
type BatchTagSpec struct {
	Filter BatchTagFilter     `json:"filter"`
	Tags   map[string]*string `json:"tags"`
}

// BatchTagFilter mirrors the GET /files selection predicate in a JSON-friendly
// form (UUIDs/status as strings, tag predicates already parsed). Empty fields are
// omitted from the selection.
type BatchTagFilter struct {
	AgentID    string         `json:"agent_id,omitempty"`
	BucketID   string         `json:"bucket_id,omitempty"`
	FileTypeID string         `json:"file_type_id,omitempty"`
	Status     string         `json:"status,omitempty"`
	Tags       []TagPredicate `json:"tags,omitempty"`
}

// TagPredicate is one key:value file_tags predicate (AND-combined in a filter).
type TagPredicate struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}
