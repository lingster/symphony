package agent

import (
	"strings"
)

// Router selects an agent based on assignee information.
type Router struct {
	registry *Registry
	defaults string
}

// NewRouter creates a new agent router.
func NewRouter(registry *Registry, defaultAgent string) *Router {
	if defaultAgent == "" {
		defaultAgent = "codex"
	}
	return &Router{
		registry: registry,
		defaults: defaultAgent,
	}
}

// Route selects an agent based on assignee name/username.
// Returns the matched agent or the default agent.
func (r *Router) Route(assigneeName, assigneeUsername string) Agent {
	name := strings.ToLower(assigneeName)
	username := strings.ToLower(assigneeUsername)

	// Check for agent keywords in assignee
	patterns := []struct {
		keyword   string
		agentName string
	}{
		{"gemini", "gemini"},
		{"claude", "claude"},
		{"codex", "codex"},
	}

	for _, p := range patterns {
		if strings.Contains(name, p.keyword) || strings.Contains(username, p.keyword) {
			if agent, ok := r.registry.Get(p.agentName); ok {
				return agent
			}
		}
	}

	// Return default
	if agent, ok := r.registry.Get(r.defaults); ok {
		return agent
	}

	// Ultimate fallback
	agent, _ := r.registry.Get("codex")
	return agent
}

// DefaultAgent returns the configured default agent.
func (r *Router) DefaultAgent() Agent {
	agent, _ := r.registry.Get(r.defaults)
	return agent
}
