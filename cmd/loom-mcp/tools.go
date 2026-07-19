package main

// The MCP tool surface loom-mcp exposes to a driving loom session (the companion):
// session_open / session_send / session_list / session_close — commission another
// loom session, converse with it, see them all, and e-stop any of them.

import (
	"context"
	"fmt"

	"github.com/cpuchip/loom"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- tool I/O shapes (the SDK reflects these into JSON Schema) ----

type SessionOpenInput struct {
	Purpose  string `json:"purpose" jsonschema:"what this session is for — a plain sentence; shown on the approval card for a writable session"`
	Backend  string `json:"backend,omitempty" jsonschema:"loom backend to open (claude, codex, agy, opencode, copilot, local); default claude"`
	Model    string `json:"model,omitempty" jsonschema:"backend model override; empty = the backend default"`
	Writable bool   `json:"writable" jsonschema:"true = a builder seat with writable islands, GATED behind your tap; false = a read-only advisory seat that opens immediately"`
	Workdir  string `json:"workdir,omitempty" jsonschema:"host path to mount as the writable build dir /commission (writable seats only); empty = a fresh per-session dir"`
}

type openResult struct {
	Handle    string `json:"handle"`
	State     string `json:"state"`
	Writable  bool   `json:"writable"`
	HingeID   int64  `json:"hinge_id,omitempty"`
	Workdir   string `json:"workdir,omitempty"`
	Scratch   string `json:"scratch,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Message   string `json:"message"`
}

type SessionSendInput struct {
	Handle  string `json:"handle" jsonschema:"the session handle from session_open"`
	Message string `json:"message" jsonschema:"the message to send to the session"`
}

type sendResult struct {
	Handle   string  `json:"handle"`
	State    string  `json:"state"`
	Reply    string  `json:"reply,omitempty"`
	Error    string  `json:"error,omitempty"`
	CostUSD  float64 `json:"cost_usd,omitempty"`
	TimedOut bool    `json:"timed_out,omitempty"`
	Message  string  `json:"message,omitempty"`
}

type SessionListInput struct{}

type sessionView struct {
	Handle     string `json:"handle"`
	Purpose    string `json:"purpose"`
	Backend    string `json:"backend"`
	Model      string `json:"model,omitempty"`
	Writable   bool   `json:"writable"`
	State      string `json:"state"`
	HingeID    int64  `json:"hinge_id,omitempty"`
	Workdir    string `json:"workdir,omitempty"`
	Scratch    string `json:"scratch,omitempty"`
	AgeSeconds int    `json:"age_seconds"`
	Note       string `json:"note,omitempty"`
}

type listResult struct {
	Sessions []sessionView `json:"sessions"`
	Active   int           `json:"active"`
	Max      int           `json:"max"`
}

type SessionCloseInput struct {
	Handle string `json:"handle" jsonschema:"the session handle to close / e-stop"`
	Reason string `json:"reason,omitempty" jsonschema:"optional reason recorded on the session"`
}

type closeResult struct {
	OK        bool   `json:"ok"`
	Handle    string `json:"handle"`
	Killed    bool   `json:"killed"`
	PrevState string `json:"prev_state"`
	Message   string `json:"message"`
}

// ---- serve-wide overview + generalized kill (the supervising surface) ----

type SessionsOverviewInput struct{}

// turnView is one recorded (prompt, reply) turn — the chat tail a supervising UI
// can expand under a session card.
type turnView struct {
	At     string `json:"at,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	Reply  string `json:"reply,omitempty"`
}

// overviewEntry is one live session anywhere on the box: a loom-mcp commission,
// a serve ws resident, a warm sticky seat, or a direct `loom run` CLI worker. Kind
// tells them apart; Handle is the kill target for any of them.
type overviewEntry struct {
	Kind        string     `json:"kind"` // "commission" | "resident" | "warm-seat" | "cli-worker"
	Handle      string     `json:"handle"`
	Name        string     `json:"name,omitempty"`
	Purpose     string     `json:"purpose,omitempty"`
	Backend     string     `json:"backend,omitempty"`
	Model       string     `json:"model,omitempty"`
	State       string     `json:"state"`
	Writable    bool       `json:"writable,omitempty"`
	HingeID     int64      `json:"hinge_id,omitempty"`
	AgeSeconds  int        `json:"age_seconds,omitempty"`
	IdleSeconds int        `json:"idle_seconds,omitempty"`
	Tail        string     `json:"tail,omitempty"`  // most recent reply text (a glance)
	Turns       []turnView `json:"turns,omitempty"` // recent chat tail (commissions)
	Note        string     `json:"note,omitempty"`
}

type overviewResult struct {
	Sessions     []overviewEntry `json:"sessions"`
	Active       int             `json:"active"`
	Max          int             `json:"max"`
	ServeError   string          `json:"serve_error,omitempty"`   // non-empty if the serve query failed (commissions still listed)
	WorkersError string          `json:"workers_error,omitempty"` // non-empty if the CLI-worker process scan failed (everything else still listed)
}

// fromServeOverview maps a serve-reported session (resident or warm seat) into
// the unified overview entry shape.
func fromServeOverview(e loom.SessionOverview) overviewEntry {
	return overviewEntry{
		Kind:        e.Kind,
		Handle:      e.Handle,
		Name:        e.Name,
		Backend:     e.Backend,
		Model:       e.Model,
		State:       e.State,
		IdleSeconds: e.IdleSeconds,
		Tail:        e.Tail,
		Note:        serveKindNote(e.Kind),
	}
}

// serveKindNote annotates the stop semantics for a serve-side session, so a
// supervising UI can warn honestly before killing it.
func serveKindNote(kind string) string {
	switch kind {
	case "warm-seat":
		return "stop downgrades this warm seat to cold-resumable (lineage kept)"
	case "resident":
		return "stop closes this resident (process dropped, remembered lineage cleared)"
	default:
		return ""
	}
}

type SessionKillInput struct {
	Target string `json:"target" jsonschema:"the session to stop — a commission handle, a resident name/handle, a warm-seat name/handle, or a CLI worker's pid target \"pid:<n>\" (all from sessions_overview)"`
	Reason string `json:"reason,omitempty" jsonschema:"optional reason recorded on a commission"`
}

type killResult struct {
	OK        bool   `json:"ok"`
	Kind      string `json:"kind,omitempty"` // what was stopped: commission | resident | warm-seat
	Target    string `json:"target"`
	Killed    bool   `json:"killed"`
	PrevState string `json:"prev_state,omitempty"`
	Message   string `json:"message"`
}

// registerTools wires the four tools onto a server, all bound to the shared manager.
func registerTools(s *mcp.Server, m *manager) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "session_open",
		Description: "Commission another loom session — a fresh Claude (or other backend) seat grounded on the " +
			"whole workspace read-only at /work, to do real work and talk back to you through session_send. " +
			"writable=false opens a read-only ADVISORY seat immediately (reading, analysis, second opinions). " +
			"writable=true opens a BUILDER seat with writable islands (/commission to build in, /scratch to journal) " +
			"— but it is GATED: a tap-to-approve card goes to Michael's phone and the seat opens only when he approves. " +
			"Cap: a small number of concurrent sessions; over cap it refuses (close one first). Prefer a Task subagent " +
			"for quick reads inside your own room; commission a session when the work needs writes and a durable seat.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SessionOpenInput) (*mcp.CallToolResult, openResult, error) {
		res, err := m.Open(ctx, openReq{
			purpose: in.Purpose, backend: in.Backend, model: in.Model, writable: in.Writable, workdir: in.Workdir,
		})
		if err != nil {
			return toolError("session_open: %v", err), openResult{}, nil
		}
		return nil, res, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "session_send",
		Description: "Send a message to a commissioned session and get its reply. If the session is still awaiting " +
			"your tap (writable), this reports that instead of sending — approve the card, then send again. " +
			"Blocks until the seat finishes its turn.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SessionSendInput) (*mcp.CallToolResult, sendResult, error) {
		res, err := m.Send(ctx, in.Handle, in.Message)
		if err != nil {
			return toolError("session_send: %v", err), sendResult{}, nil
		}
		return nil, res, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "session_list",
		Description: "List every commissioned session and its state (awaiting_approval, open, declined, closed), plus the active-vs-cap count.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ SessionListInput) (*mcp.CallToolResult, listResult, error) {
		return nil, m.List(), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "session_close",
		Description: "Close (EMERGENCY-STOP) a commissioned session by handle — kills its running seat immediately. " +
			"Works on ANY commissioned session, not only ones you opened. A session still awaiting approval is " +
			"withdrawn (its tap card clears).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SessionCloseInput) (*mcp.CallToolResult, closeResult, error) {
		res, err := m.Close(ctx, in.Handle, in.Reason)
		if err != nil {
			return toolError("session_close: %v", err), closeResult{}, nil
		}
		return nil, res, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "sessions_overview",
		Description: "The WHOLE-BOX view: every live loom session on this box — loom-mcp commissions, ws " +
			"residents (named warm seats), the OpenAI-shim warm sticky seats (e.g. the voice companion), AND " +
			"the direct `loom run` CLI workers a foreman launched on this box (kind=\"cli-worker\") — each with " +
			"its kind, model/backend, state, idle time, and a short recent chat tail to glance at. " +
			"Use this to SEE what is running before deciding to stop any of it; each entry's `handle` is the " +
			"target for session_kill. (Commissions show their full purpose + recent turns; warm seats and " +
			"cli-workers carry NO transcript — loom-mcp doesn't drive them.) If the serve is unreachable, " +
			"commissions + CLI workers still list and `serve_error` is set; if the process scan fails, everything " +
			"else still lists and `workers_error` is set.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ SessionsOverviewInput) (*mcp.CallToolResult, overviewResult, error) {
		return nil, m.Overview(ctx), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "session_kill",
		Description: "Stop ANY loom session on this box by name, handle, or pid — the generalized e-stop. A commission " +
			"is killed (seat dropped, pending tap withdrawn, scratch cleaned); a ws resident is closed (process " +
			"dropped, remembered lineage cleared); a warm sticky seat is DOWNGRADED to cold-resumable (its live " +
			"process torn down but its conversation lineage kept, so it can resume cold); a direct CLI worker " +
			"(target \"pid:<n>\") is FORCE-KILLED with its agent subprocess tree — irreversible, mid-task, so " +
			"confirm before calling it. Pass a `target` from sessions_overview. Reports what kind was stopped and " +
			"the exact semantics applied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SessionKillInput) (*mcp.CallToolResult, killResult, error) {
		res, err := m.Kill(ctx, in.Target, in.Reason)
		if err != nil {
			return toolError("session_kill: %v", err), killResult{}, nil
		}
		return nil, res, nil
	})
}

// toolError builds a tool-execution error result (isError:true) — the model sees
// it and can react, distinct from a JSON-RPC protocol error.
func toolError(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// ---- the real opener: a ws client to a loom serve ----

// connectOpener opens sessions on a loom serve over the native ws protocol,
// reusing loom's own ConnectBackend client machinery. Sessions are EPHEMERAL
// (no SessionName): loom-mcp holds the socket for the session's life, and Close
// asks the serve to DROP the process (keep_alive=false) — that is the e-stop.
type connectOpener struct {
	url   string
	token string
}

func (o connectOpener) open(ctx context.Context, backend string, opts loom.SessionOpts) (loom.Session, error) {
	b := loom.ConnectBackend{URL: o.url, Token: o.token, Agent: backend}
	return b.Open(ctx, opts)
}

// overview lists the serve's OTHER live sessions (residents + warm sticky
// seats). Session-less: it needs only the token, not a backend.
func (o connectOpener) overview(ctx context.Context) ([]loom.SessionOverview, error) {
	return loom.ConnectBackend{URL: o.url, Token: o.token}.Overview(ctx)
}

// kill stops a serve session by name or handle (resident or warm seat).
func (o connectOpener) kill(ctx context.Context, target string) (kind, note string, found bool, err error) {
	return loom.ConnectBackend{URL: o.url, Token: o.token}.Kill(ctx, target)
}
