// Package handler provides internal (white-box) tests for unexported helpers.
package handler

import (
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/stretchr/testify/assert"
)

// TestMapFrontendStatusToDB verifies the mapping from the Web UI / SDK
// "frontend" status vocabulary (uppercase, RUNNING for online agents) to the
// DB enum values (lowercase, "online" for connected agents).
func TestMapFrontendStatusToDB(t *testing.T) {
	cases := []struct {
		input string
		want  db.AgentStatus
	}{
		// Normal uppercase mappings
		{"PENDING", db.AgentStatusPending},
		{"APPROVED", db.AgentStatusApproved},
		{"OFFLINE", db.AgentStatusOffline},
		{"REVOKED", db.AgentStatusRevoked},
		// RUNNING → online is the critical mapping fixed by T3-2-FIX-C:
		// the DB stores "online" for connected agents, but the Web UI/SDK
		// uses "RUNNING" — mismatching this caused approve buttons to vanish.
		{"RUNNING", db.AgentStatusOnline},
		// Already lowercase inputs are idempotent
		{"pending", db.AgentStatusPending},
		{"online", db.AgentStatusOnline},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := mapFrontendStatusToDB(tc.input)
			assert.Equal(t, tc.want, got, "mapFrontendStatusToDB(%q)", tc.input)
		})
	}
}

// TestMapDBStatusToFrontend verifies the reverse mapping from DB enum values
// (lowercase) to the Web UI / SDK vocabulary (uppercase, RUNNING for online).
func TestMapDBStatusToFrontend(t *testing.T) {
	cases := []struct {
		input db.AgentStatus
		want  string
	}{
		// Critical: online → RUNNING (not "ONLINE")
		{db.AgentStatusOnline, "RUNNING"},
		// Normal lowercase → uppercase
		{db.AgentStatusPending, "PENDING"},
		{db.AgentStatusApproved, "APPROVED"},
		{db.AgentStatusOffline, "OFFLINE"},
		{db.AgentStatusRevoked, "REVOKED"},
	}

	for _, tc := range cases {
		t.Run(string(tc.input), func(t *testing.T) {
			got := mapDBStatusToFrontend(tc.input)
			assert.Equal(t, tc.want, got, "mapDBStatusToFrontend(%q)", tc.input)
		})
	}
}

// TestStatusMappingRoundTrip verifies that DB→frontend→DB is a lossless round-
// trip for all known status values.  This catches regressions where a new
// status enum value is added without updating both mapping functions.
func TestStatusMappingRoundTrip(t *testing.T) {
	knownDBStatuses := []db.AgentStatus{
		db.AgentStatusPending,
		db.AgentStatusApproved,
		db.AgentStatusOnline,
		db.AgentStatusOffline,
		db.AgentStatusRevoked,
	}

	for _, s := range knownDBStatuses {
		t.Run(string(s), func(t *testing.T) {
			frontend := mapDBStatusToFrontend(s)
			backToDBs := mapFrontendStatusToDB(frontend)
			assert.Equal(t, s, backToDBs,
				"round-trip for DB status %q: DB→frontend(%q)→DB produced %q",
				s, frontend, backToDBs,
			)
		})
	}
}
