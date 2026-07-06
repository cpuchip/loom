package loom

// openai.go — an OpenAI-compatible /v1/chat/completions endpoint on the serve
// listener, so `loom serve` is a dispatchable MODEL PROVIDER, not only a
// harness dispatcher. pg-ai-stewards registers a `loom` provider pointing here;
// then ANY model alias (critic, review, or eventually all of them) can resolve
// to a harness-driven model — e.g. Claude sonnet via a Max subscription —
// instead of a paid per-token API. The request drives a fresh one-shot backend
// session (default agent claude) and returns its reply in OpenAI shape.
//
// Statelessness: a chat-completions call carries its whole history each time
// (the caller composes it). We flatten those messages into ONE prompt for a
// fresh isolated session. That fits single-shot uses (critique, review,
// judge) cleanly; it is intentionally NOT a multi-turn tool loop — for that,
// drive the native ws protocol.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// openaiClaudeHome, if set via `loom serve --openai-claude-home`, is the DEFAULT
// ~/.claude the shim's isolated sessions mount (skills/settings/MCP). Empty =
// loom's default. Package-level because the serve listener is constructed
// without plumbing extra opts.
var openaiClaudeHome string

// openaiHomeRoot, if set via `loom serve --openai-home-root`, enables ROLE-aware
// environments: a model named "<model>#<role>" (e.g. "sonnet#critic") mounts
// <root>/<role>-claude-home instead of the default. This lets one loom serve
// host purpose-built environments — a code-review home, an argument-critique
// home, a plain home — each with its own CLAUDE.md/skills, selected per request
// by the model name. A bare "sonnet" or an unknown role uses the default home.
var openaiHomeRoot string

// openaiMCPConfig, if set via `loom serve --openai-mcp-config`, is the
// --mcp-config JSON handed to every shim-spawned session — the hinge back into
// pg-ai-stewards (doc_*, doc_search, …). Without it a shim critic is a plain
// Claude Code with no substrate tools, so a doc-construction critique/finalize
// stage cannot read or pool the draft. NOTE: an isolated session runs in a Linux
// container, so the config's server must be reachable FROM the container (a
// container-baked binary or an http endpoint via host.docker.internal), and any
// DSN must resolve there too.
var openaiMCPConfig string

// openaiTimeout caps one shim completion end-to-end (session spawn → reply).
// The shim's context is also tied to the HTTP request, so the EFFECTIVE
// ceiling is min(this, the caller's own client timeout) — pg-ai-stewards'
// bgworker defaults to 1800s (STEWARDS_CHAT_TIMEOUT_SECONDS), so keep this
// ABOVE the caller's or the caller governs. The old hardcoded 10 minutes
// silently killed grounded high-effort sessions mid-work while the caller's
// retry restarted them from zero — an invisible spend loop (#334).
var openaiTimeout = 30 * time.Minute

// SetOpenAIClaudeHome sets the default ~/.claude the OpenAI shim mounts.
func SetOpenAIClaudeHome(home string) { openaiClaudeHome = home }

// SetOpenAIMCPConfig sets the --mcp-config JSON path handed to shim sessions.
func SetOpenAIMCPConfig(path string) { openaiMCPConfig = path }

// SetOpenAITimeout sets the per-completion wall clock (`--openai-timeout`).
func SetOpenAITimeout(d time.Duration) {
	if d > 0 {
		openaiTimeout = d
	}
}

// SetOpenAIHomeRoot sets the directory that holds role-specific claude-homes
// (<root>/<role>-claude-home), selected by a "<model>#<role>" model name.
func SetOpenAIHomeRoot(root string) { openaiHomeRoot = root }

// resolveModelHome splits a "model#role" name into the bare claude model, the
// role-specific claude-home, and the role-specific WORKDIR.
// "sonnet#critic" -> ("sonnet", <root>/critic-claude-home, <root>/critic-workdir).
// A bare "sonnet", an empty role, or a role whose home dir is missing all fall
// back to the default home with the model unchanged (a config gap never fails a
// request — it just loses the specialization).
//
// The workdir is the role's CONTEXT: an optional sibling directory
// (<root>/<role>-workdir) bind-mounted READ-ONLY as /work in the isolated
// session. Without it a shim session is a model in a bare room — it can reason
// but cannot ground (the pg-ai-stewards war-game seat's first artifacts had
// assumptions ledgers dominated by "source unreadable" for exactly this
// reason). The mount is ro because a shim seat reads context and writes ONLY
// through its MCP hinge; the workdir is never an exfil channel. Missing dir =
// no mount (workdir ""), same never-fail degradation as the home.
func resolveModelHome(model string) (bareModel, home, workdir string) {
	base, role, found := strings.Cut(model, "#")
	if !found || role == "" || openaiHomeRoot == "" {
		return model, openaiClaudeHome, ""
	}
	h := filepath.Join(openaiHomeRoot, role+"-claude-home")
	if fi, err := os.Stat(h); err != nil || !fi.IsDir() {
		return base, openaiClaudeHome, ""
	}
	w := filepath.Join(openaiHomeRoot, role+"-workdir")
	if fi, err := os.Stat(w); err != nil || !fi.IsDir() {
		return base, h, ""
	}
	return base, h, w
}

// openaiChatReq is the subset of the OpenAI request body we honor.
type openaiChatReq struct {
	Model    string          `json:"model"`
	Messages []openaiMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	// User is OpenAI's standard end-user identifier. pg-ai-stewards sets it to
	// the dispatching stage's session id (wi--<uuid8>--<stage>); the shim
	// forwards it into the session's MCP config as an X-Stewards-Session
	// header so the substrate can scope the session's doc drafts to the work
	// item it serves (#333 — provenance for all-loom doc-construction).
	User string `json:"user"`
}

// sessionMCPConfig derives a per-session MCP config from the role home's base
// config, injecting an X-Stewards-Session header into every server entry. It
// returns the IN-CONTAINER path to pass as --mcp-config plus the HOST path for
// cleanup. Any failure returns ("", "") — the session falls back to the static
// config and merely loses provenance, never the request.
//
// Path mapping: baseInContainer is an in-container path (the mounted home is
// /home/node/.claude); its basename must exist in the HOST home dir. The
// derived file is written under <home>/.session-mcp/ (host) which the session
// sees at /home/node/.claude/.session-mcp/ (the home is the mount).
func sessionMCPConfig(hostHome, baseInContainer, session string) (inContainer, hostPath string) {
	if hostHome == "" || baseInContainer == "" || session == "" {
		return "", ""
	}
	raw, err := os.ReadFile(filepath.Join(hostHome, filepath.Base(filepath.ToSlash(baseInContainer))))
	if err != nil {
		return "", ""
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", ""
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if len(servers) == 0 {
		return "", ""
	}
	for _, v := range servers {
		srv, ok := v.(map[string]any)
		if !ok {
			continue
		}
		headers, _ := srv["headers"].(map[string]any)
		if headers == nil {
			headers = map[string]any{}
		}
		headers["X-Stewards-Session"] = session
		srv["headers"] = headers
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", ""
	}
	dir := filepath.Join(hostHome, ".session-mcp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", ""
	}
	f, err := os.CreateTemp(dir, "mcp-*.json")
	if err != nil {
		return "", ""
	}
	hostPath = f.Name()
	if _, err := f.Write(out); err != nil {
		f.Close()
		os.Remove(hostPath)
		return "", ""
	}
	f.Close()
	return "/home/node/.claude/.session-mcp/" + filepath.Base(hostPath), hostPath
}

type openaiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string, or array of content parts
}

// serveOpenAI answers POST /v1/chat/completions.
func (s *server) serveOpenAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"method not allowed"}}`, http.StatusMethodNotAllowed)
		return
	}
	// Auth posture: the OpenAI endpoint is reachable only on the serve
	// listener's bind address, which is loopback in every real deployment
	// (127.0.0.1, reached from a container via host.docker.internal → host
	// loopback). Localhost is the wall — the same posture the substrate's own
	// MCP HTTP surface uses — so a caller may register loom as a KEYLESS
	// provider (no encrypted secret). A Bearer token is still honored if sent
	// (defense in depth); it's just not required, so provider wiring stays
	// dial-only. Do NOT bind the serve listener to 0.0.0.0 with the shim on.
	if tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); tok != "" && s.requireToken && !s.tokens.verify(tok) {
		http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
		return
	}
	var req openaiChatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"bad request: `+err.Error()+`"}}`, http.StatusBadRequest)
		return
	}

	agent := "claude"
	be, ok := s.backends[agent]
	if !ok {
		writeOpenAIErr(w, req.Stream, fmt.Errorf("no %q backend on this loom serve", agent))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), openaiTimeout)
	defer cancel()

	// Role-aware environment: "sonnet#critic" -> sonnet + the critic home
	// (+ the critic workdir mounted ro as /work, if <root>/critic-workdir exists).
	bareModel, home, workdir := resolveModelHome(req.Model)

	// #333: a substrate dispatch declares its session in the standard `user`
	// field — derive a per-session MCP config carrying it as a header, so the
	// session's doc drafts scope to the work item. Fallback = static config.
	mcpCfg := openaiMCPConfig
	if strings.HasPrefix(req.User, "wi--") {
		if derived, host := sessionMCPConfig(home, openaiMCPConfig, req.User); derived != "" {
			mcpCfg = derived
			defer os.Remove(host)
		}
	}

	sess, err := be.Open(ctx, SessionOpts{
		Model:           bareModel, // loom passes --model straight through
		Isolate:         true,      // clean sandbox per review; no host-config bleed
		SkipPermissions: true,
		ClaudeHome:      home,
		Workdir:         workdir, // role context ("" = serve's own cwd, the historical default)
		WorkdirRO:       workdir != "",
		MCPConfig:       mcpCfg, // hinge into pg-ai-stewards (doc_* etc.), if configured
	})
	if err != nil {
		writeOpenAIErr(w, req.Stream, fmt.Errorf("open session: %w", err))
		return
	}
	defer sess.Close()

	reply, err := sess.Send(ctx, flattenMessages(req.Messages))
	if err != nil {
		writeOpenAIErr(w, req.Stream, fmt.Errorf("send: %w", err))
		return
	}
	if reply.Err != "" {
		writeOpenAIErr(w, req.Stream, fmt.Errorf("%s", reply.Err))
		return
	}

	if req.Stream {
		writeOpenAISSE(w, req.Model, reply.Text)
	} else {
		writeOpenAIJSON(w, req.Model, reply.Text)
	}
}

// flattenMessages renders an OpenAI message list as a single labeled prompt.
// A leading system block becomes an unlabeled preamble; the rest are role-tagged.
func flattenMessages(msgs []openaiMessage) string {
	var b strings.Builder
	for i, m := range msgs {
		text := contentText(m.Content)
		if text == "" {
			continue
		}
		switch m.Role {
		case "system":
			b.WriteString(text)
			b.WriteString("\n\n")
		case "assistant":
			b.WriteString("\n[assistant]\n")
			b.WriteString(text)
			b.WriteString("\n")
		default: // user, tool, etc.
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// contentText extracts text from an OpenAI content field, which is either a
// plain string or an array of {type:"text",text:...} parts.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// writeOpenAISSE emits the reply as a minimal OpenAI streaming response: one
// role delta, one content delta carrying the whole text, a stop finish, [DONE].
// A single content chunk satisfies the accumulate-deltas parser on the caller.
func writeOpenAISSE(w http.ResponseWriter, model, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	id := "chatcmpl-loom"
	emit := func(v any) {
		bs, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", bs)
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit(map[string]any{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}}})
	emit(map[string]any{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}})
	emit(map[string]any{"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// writeOpenAIJSON emits a non-streaming chat.completion (for a stream:false caller).
func writeOpenAIJSON(w http.ResponseWriter, model, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-loom", "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": text}}},
	})
}

// writeOpenAIErr surfaces an error in the shape the caller expects: an SSE error
// frame for a streaming request, else a JSON error body.
func writeOpenAIErr(w http.ResponseWriter, stream bool, err error) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		bs, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error()}})
		fmt.Fprintf(w, "data: %s\n\n", bs)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	bs, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error()}})
	_, _ = w.Write(bs)
}
