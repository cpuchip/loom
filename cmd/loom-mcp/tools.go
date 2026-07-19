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
