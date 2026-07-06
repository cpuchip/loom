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
	Workdir   string // process working dir ("" = inherit)
	WorkdirRO bool   // (--isolate) mount Workdir as /work READ-ONLY — for context-only seats (shim role workdirs) whose only write channel is their MCP hinge
	Model     string // backend-specific model override ("" = default)
	Isolate bool   // run the agent in a docker sandbox (claude backend) — walls the host
	Image   string // docker image for isolation ("" = loom-claude)
	Remote  string // run the agent on a remote box over ssh (e.g. "cpuchip@host"); "" = local
	Resume  string // resume a prior session by id (claude --resume); "" = fresh session

	// Configuring the claude agent — the substrate-integration surface. Paths in the
	// config-file fields are interpreted on the TARGET (local host / remote box /
	// inside the container via ClaudeHome), so put them where the agent will run.
	MCPConfig        string // claude --mcp-config: wire in MCP server(s) from JSON — the hinge back into pg-ai-stewards
	AllowedTools     string // claude --allowed-tools: scope which tools (incl. MCP) the agent may call
	PermissionMode   string // claude --permission-mode (e.g. "acceptEdits", "plan")
	SkipPermissions  bool   // claude --dangerously-skip-permissions (headless; safe INSIDE --isolate)
	SystemPromptFile string // claude --append-system-prompt-file: inject instructions
	ClaudeHome       string // (--isolate) host dir mounted as the container's writable ~/.claude: skills/instructions/settings/MCP + PERSISTED session state (this is what makes resume+isolate work)
	Consult          bool   // read-only "consult" drive: inject a directive so a QUESTION drive doesn't sprawl into edits/commits/journaling (instruction-level, not a hard sandbox — use AllowedTools for enforcement)
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

// Interruptible is an optional capability: a session whose IN-FLIGHT turn can be
// stopped while the agent is working (claude's stream-json control_request
// interrupt). The session stays alive — to steer, call Send with a new
// instruction after Interrupt returns; the context is intact. Callers type-assert
// for it: `if it, ok := sess.(Interruptible); ok { it.Interrupt() }`.
type Interruptible interface {
	Interrupt() error
}

// Backends returns the built-in backend registry keyed by name.
func Backends() map[string]Backend {
	return map[string]Backend{
		"claude": ClaudeBackend{Bin: "claude"},
		"codex":  CodexBackend{Bin: "codex"},
		"agy":    DefaultAgyBackend(),
		"local":  DefaultLocalBackend(),
	}
}
