package trollsift

// AgentContext carries agent-specific metadata for injection into trollsift values.
type AgentContext struct {
	AgentName string // injected as {agent_name}
	AgentID   string // injected as {agent_id}
}

// InjectContext merges AgentContext fields into vals.
// Existing keys in vals are never overwritten.
func InjectContext(ctx AgentContext, vals map[string]Value) map[string]Value {
	if vals == nil {
		vals = make(map[string]Value)
	}
	if ctx.AgentName != "" {
		if _, exists := vals["agent_name"]; !exists {
			vals["agent_name"] = S(ctx.AgentName)
		}
	}
	if ctx.AgentID != "" {
		if _, exists := vals["agent_id"]; !exists {
			vals["agent_id"] = S(ctx.AgentID)
		}
	}
	return vals
}
