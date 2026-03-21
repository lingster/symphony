// Package agent defines the abstract interface for coding agent backends.
package agent

import (
	"context"
	"encoding/json"
)

// Agent represents a coding agent backend (Codex, Claude Code, Gemini CLI).
type Agent interface {
	// Name returns the agent's identifier.
	Name() string

	// Start launches an agent session in the given workspace with the prompt.
	// systemPrompt is optional additional context appended to the system prompt.
	Start(ctx context.Context, workspace string, prompt string, systemPrompt string) (Session, error)
}

// Session represents an active agent session.
type Session interface {
	// Events returns a channel that receives events from the agent.
	Events() <-chan Event

	// Send sends input to the agent (for interactive sessions).
	Send(input string) error

	// Stop terminates the agent session.
	Stop() error

	// Wait blocks until the session completes.
	Wait() error
}

// EventType represents the type of agent event.
type EventType string

const (
	EventTypeMessage    EventType = "message"
	EventTypeToolUse    EventType = "tool_use"
	EventTypeToolResult EventType = "tool_result"
	EventTypeError      EventType = "error"
	EventTypeComplete   EventType = "complete"
)

// Event represents an event emitted by the agent.
type Event struct {
	Type    EventType       `json:"type"`
	Content string          `json:"content"`
	Raw     json.RawMessage `json:"raw,omitempty"`
}

// Registry holds available agent implementations.
type Registry struct {
	agents map[string]Agent
}

// NewRegistry creates a new agent registry.
func NewRegistry() *Registry {
	return &Registry{
		agents: make(map[string]Agent),
	}
}

// Register adds an agent to the registry.
func (r *Registry) Register(agent Agent) {
	r.agents[agent.Name()] = agent
}

// Get retrieves an agent by name.
func (r *Registry) Get(name string) (Agent, bool) {
	agent, ok := r.agents[name]
	return agent, ok
}

// List returns all registered agent names.
func (r *Registry) List() []string {
	names := make([]string, 0, len(r.agents))
	for name := range r.agents {
		names = append(names, name)
	}
	return names
}
