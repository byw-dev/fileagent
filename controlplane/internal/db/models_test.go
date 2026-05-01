package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── ActionType ────────────────────────────────────────────────────────────────

func TestActionType_Scan_String(t *testing.T) {
	var a ActionType
	require.NoError(t, a.Scan("webhook"))
	assert.Equal(t, ActionTypeWebhook, a)
}

func TestActionType_Scan_Bytes(t *testing.T) {
	var a ActionType
	require.NoError(t, a.Scan([]byte("nats_publish")))
	assert.Equal(t, ActionTypeNatsPublish, a)
}

func TestActionType_Scan_Invalid(t *testing.T) {
	var a ActionType
	require.Error(t, a.Scan(42))
}

func TestNullActionType_Scan_Nil(t *testing.T) {
	var n NullActionType
	require.NoError(t, n.Scan(nil))
	assert.False(t, n.Valid)
}

func TestNullActionType_Scan_Valid(t *testing.T) {
	var n NullActionType
	require.NoError(t, n.Scan("webhook"))
	assert.True(t, n.Valid)
	assert.Equal(t, ActionTypeWebhook, n.ActionType)
}

func TestNullActionType_Value_Valid(t *testing.T) {
	n := NullActionType{ActionType: ActionTypeWebhook, Valid: true}
	v, err := n.Value()
	require.NoError(t, err)
	assert.Equal(t, "webhook", v)
}

func TestNullActionType_Value_Invalid(t *testing.T) {
	n := NullActionType{Valid: false}
	v, err := n.Value()
	require.NoError(t, err)
	assert.Nil(t, v)
}

// ── AgentStatus ───────────────────────────────────────────────────────────────

func TestAgentStatus_Scan_String(t *testing.T) {
	var s AgentStatus
	require.NoError(t, s.Scan("pending"))
	assert.Equal(t, AgentStatusPending, s)
}

func TestAgentStatus_Scan_Bytes(t *testing.T) {
	var s AgentStatus
	require.NoError(t, s.Scan([]byte("approved")))
	assert.Equal(t, AgentStatusApproved, s)
}

func TestAgentStatus_Scan_Invalid(t *testing.T) {
	var s AgentStatus
	require.Error(t, s.Scan(3.14))
}

func TestNullAgentStatus_Scan_Nil(t *testing.T) {
	var n NullAgentStatus
	require.NoError(t, n.Scan(nil))
	assert.False(t, n.Valid)
}

func TestNullAgentStatus_Scan_Valid(t *testing.T) {
	var n NullAgentStatus
	require.NoError(t, n.Scan("online"))
	assert.True(t, n.Valid)
	assert.Equal(t, AgentStatusOnline, n.AgentStatus)
}

func TestNullAgentStatus_Value_Valid(t *testing.T) {
	n := NullAgentStatus{AgentStatus: AgentStatusRevoked, Valid: true}
	v, err := n.Value()
	require.NoError(t, err)
	assert.Equal(t, "revoked", v)
}

func TestNullAgentStatus_Value_Invalid(t *testing.T) {
	n := NullAgentStatus{Valid: false}
	v, err := n.Value()
	require.NoError(t, err)
	assert.Nil(t, v)
}

// ── EventType ─────────────────────────────────────────────────────────────────

func TestEventType_Scan_String(t *testing.T) {
	var e EventType
	require.NoError(t, e.Scan("file_uploaded"))
	assert.Equal(t, EventTypeFileUploaded, e)
}

func TestEventType_Scan_Bytes(t *testing.T) {
	var e EventType
	require.NoError(t, e.Scan([]byte("agent_online")))
	assert.Equal(t, EventTypeAgentOnline, e)
}

func TestEventType_Scan_Invalid(t *testing.T) {
	var e EventType
	require.Error(t, e.Scan(true))
}

func TestNullEventType_Scan_Nil(t *testing.T) {
	var n NullEventType
	require.NoError(t, n.Scan(nil))
	assert.False(t, n.Valid)
}

func TestNullEventType_Scan_Valid(t *testing.T) {
	var n NullEventType
	require.NoError(t, n.Scan("agent_offline"))
	assert.True(t, n.Valid)
	assert.Equal(t, EventTypeAgentOffline, n.EventType)
}

func TestNullEventType_Value_Valid(t *testing.T) {
	n := NullEventType{EventType: EventTypeFileDeleted, Valid: true}
	v, err := n.Value()
	require.NoError(t, err)
	assert.Equal(t, "file_deleted", v)
}

func TestNullEventType_Value_Invalid(t *testing.T) {
	n := NullEventType{Valid: false}
	v, err := n.Value()
	require.NoError(t, err)
	assert.Nil(t, v)
}

// ── FileStatus ────────────────────────────────────────────────────────────────

func TestFileStatus_Scan_String(t *testing.T) {
	var s FileStatus
	require.NoError(t, s.Scan("completed"))
	assert.Equal(t, FileStatusCompleted, s)
}

func TestFileStatus_Scan_Bytes(t *testing.T) {
	var s FileStatus
	require.NoError(t, s.Scan([]byte("failed")))
	assert.Equal(t, FileStatusFailed, s)
}

func TestFileStatus_Scan_Invalid(t *testing.T) {
	var s FileStatus
	require.Error(t, s.Scan([]int{1, 2}))
}

func TestNullFileStatus_Scan_Nil(t *testing.T) {
	var n NullFileStatus
	require.NoError(t, n.Scan(nil))
	assert.False(t, n.Valid)
}

func TestNullFileStatus_Scan_Valid(t *testing.T) {
	var n NullFileStatus
	require.NoError(t, n.Scan("uploading"))
	assert.True(t, n.Valid)
	assert.Equal(t, FileStatusUploading, n.FileStatus)
}

func TestNullFileStatus_Value_Valid(t *testing.T) {
	n := NullFileStatus{FileStatus: FileStatusDeleted, Valid: true}
	v, err := n.Value()
	require.NoError(t, err)
	assert.Equal(t, "deleted", v)
}

func TestNullFileStatus_Value_Invalid(t *testing.T) {
	n := NullFileStatus{Valid: false}
	v, err := n.Value()
	require.NoError(t, err)
	assert.Nil(t, v)
}

// ── RuleStatus ────────────────────────────────────────────────────────────────

func TestRuleStatus_Scan_String(t *testing.T) {
	var s RuleStatus
	require.NoError(t, s.Scan("active"))
	assert.Equal(t, RuleStatusActive, s)
}

func TestRuleStatus_Scan_Bytes(t *testing.T) {
	var s RuleStatus
	require.NoError(t, s.Scan([]byte("inactive")))
	assert.Equal(t, RuleStatusInactive, s)
}

func TestRuleStatus_Scan_Invalid(t *testing.T) {
	var s RuleStatus
	require.Error(t, s.Scan(struct{}{}))
}

func TestNullRuleStatus_Scan_Nil(t *testing.T) {
	var n NullRuleStatus
	require.NoError(t, n.Scan(nil))
	assert.False(t, n.Valid)
}

func TestNullRuleStatus_Scan_Valid(t *testing.T) {
	var n NullRuleStatus
	require.NoError(t, n.Scan("active"))
	assert.True(t, n.Valid)
	assert.Equal(t, RuleStatusActive, n.RuleStatus)
}

func TestNullRuleStatus_Value_Valid(t *testing.T) {
	n := NullRuleStatus{RuleStatus: RuleStatusInactive, Valid: true}
	v, err := n.Value()
	require.NoError(t, err)
	assert.Equal(t, "inactive", v)
}

func TestNullRuleStatus_Value_Invalid(t *testing.T) {
	n := NullRuleStatus{Valid: false}
	v, err := n.Value()
	require.NoError(t, err)
	assert.Nil(t, v)
}
