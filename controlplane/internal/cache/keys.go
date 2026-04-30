package cache

import "fmt"

// Key formatting functions for all Redis keys used by the Control Plane.
// Centralising key patterns here makes auditing and renaming straightforward.
//
// Key conventions (from system-design.md §3.5):
//
//   - agent:{id}:online        STRING  TTL=heartbeat×3   Agent online presence
//   - agent:{id}:sts           HASH    TTL=STS expiry    Current STS credential summary
//   - jwt:jti:{jti}            STRING  TTL=Token TTL     Revoked JWT blacklist
//   - ratelimit:api:{user_id}  STRING  TTL=1min          API rate-limit counter
//   - lock:task:{rule_id}      STRING  TTL=task timeout  Distributed task lock
//   - session:{token}          HASH    TTL=30min         Web UI user session (optional)

// AgentOnlineKey returns the Redis key used to track whether an Agent is online.
// The value is "1" and the key expires after heartbeat × 3 (default 90s).
func AgentOnlineKey(agentID string) string {
	return fmt.Sprintf("agent:%s:online", agentID)
}

// AgentSTSKey returns the Redis key used to cache the STS credential summary
// for an Agent. The key is a HASH and expires when the STS credential expires.
func AgentSTSKey(agentID string) string {
	return fmt.Sprintf("agent:%s:sts", agentID)
}

// JWTBlacklistKey returns the Redis key for a revoked JWT token identified by
// its jti (JWT ID) claim. Presence of this key means the token is revoked.
func JWTBlacklistKey(jti string) string {
	return fmt.Sprintf("jwt:jti:%s", jti)
}

// RateLimitKey returns the Redis key for the per-user API rate-limit counter.
func RateLimitKey(userID string) string {
	return fmt.Sprintf("ratelimit:api:%s", userID)
}

// LockTaskKey returns the Redis key for the distributed lock on a task
// (collection rule dispatch). The lock prevents multiple Control Plane
// instances from dispatching the same rule simultaneously.
func LockTaskKey(ruleID string) string {
	return fmt.Sprintf("lock:task:%s", ruleID)
}

// LockRuleDispatchKey returns the Redis key for the rule-dispatch lock
// described in system-design.md §5.6.
func LockRuleDispatchKey(ruleID string) string {
	return fmt.Sprintf("lock:rule_dispatch:%s", ruleID)
}

// SessionKey returns the Redis key for an optional Web UI user session.
func SessionKey(token string) string {
	return fmt.Sprintf("session:%s", token)
}
