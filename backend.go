// Package loom drives multiple coding-agent CLIs (Claude Code, agy/Gemini, …) as
// long-lived workers behind one interface — a harness around the harnesses.
//
// A weaving harness is literally a loom component, and a loom holds many
// harnesses at once; loom holds many agent CLIs and weaves their work together.
package loom

import "context"

// Reply is the result of one user turn.
type Reply struct {
	Backend   string  `json:"backend"`
	Text      string  `json:"text"`
	SessionID string  `json:"session_id,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"` // cost of THIS turn (delta), best-effort
	Turns     int     `json:"turns,omitempty"`
	Err       string  `json:"error,omitempty"`
}

// SessionOpts configure a session.
type SessionOpts struct {
	Workdir string // process working dir ("" = inherit)
	Model   string // backend-specific model override ("" = default)
}

// Backend is a driveable agent CLI.
type Backend interface {
	Name() string
	Open(ctx context.Context, opts SessionOpts) (Session, error)
}

// Session is a (possibly long-lived) conversation with one agent. Send may be
// called repeatedly; the session holds context across turns where the backend
// supports it.
//
// Send is the simple final-text path. SendStream is the same, but invokes onEvent
// for each intermediate event (assistant text, thinking, tool calls/results) as
// it arrives — so a caller can observe the agent's work, not just its conclusion.
// Send is conventionally implemented as SendStream(ctx, prompt, nil).
type Session interface {
	Send(ctx context.Context, prompt string) (Reply, error)
	SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error)
	SessionID() string
	Close() error
}

// Backends returns the built-in backend registry keyed by name.
func Backends() map[string]Backend {
	return map[string]Backend{
		"claude": ClaudeBackend{Bin: "claude"},
		"agy":    DefaultAgyBackend(),
		"local":  DefaultLocalBackend(),
	}
}
