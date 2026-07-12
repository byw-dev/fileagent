// Package retag defines the contract between retag-job enqueuers (the REST API
// handlers) and the worker that drains them (worker.RetagWorker): the job kinds
// and the spec JSON shapes carried in retag_jobs.spec. Keeping it in one small
// package lets both sides share the exact shape without importing each other.
package retag

import "github.com/google/uuid"

// Job kinds stored in retag_jobs.kind. MT-5a implements KindMerge; KindBatchTag
// and rule-retriggered retag land in MT-5b on the same channel.
const (
	KindMerge = "merge"
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
