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

// SetOpenAIClaudeHome sets the default ~/.claude the OpenAI shim mounts.
func SetOpenAIClaudeHome(home string) { openaiClaudeHome = home }

// SetOpenAIMCPConfig sets the --mcp-config JSON path handed to shim sessions.
func SetOpenAIMCPConfig(path string) { openaiMCPConfig = path }

// SetOpenAIHomeRoot sets the directory that holds role-specific claude-homes
// (<root>/<role>-claude-home), selected by a "<model>#<role>" model name.
func SetOpenAIHomeRoot(root string) { openaiHomeRoot = root }

// resolveModelHome splits a "model#role" name into the bare claude model and the
// role-specific claude-home. "sonnet#critic" -> ("sonnet", <root>/critic-claude-home).
// A bare "sonnet", an empty role, or a role whose home dir is missing all fall
// back to the default home with the model unchanged (a config gap never fails a
// request — it just loses the specialization).
func resolveModelHome(model string) (bareModel, home string) {
	base, role, found := strings.Cut(model, "#")
	if !found || role == "" || openaiHomeRoot == "" {
		return model, openaiClaudeHome
	}
	h := filepath.Join(openaiHomeRoot, role+"-claude-home")
	if fi, err := os.Stat(h); err != nil || !fi.IsDir() {
		return base, openaiClaudeHome
	}
	return base, h
}

// openaiChatReq is the subset of the OpenAI request body we honor.
type openaiChatReq struct {
	Model    string          `json:"model"`
	Messages []openaiMessage `json:"messages"`
	Stream   bool            `json:"stream"`
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

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	// Role-aware environment: "sonnet#critic" -> sonnet + the critic home.
	bareModel, home := resolveModelHome(req.Model)
	sess, err := be.Open(ctx, SessionOpts{
		Model:           bareModel, // loom passes --model straight through
		Isolate:         true,      // clean sandbox per review; no host-config bleed
		SkipPermissions: true,
		ClaudeHome:      home,
		MCPConfig:       openaiMCPConfig, // hinge into pg-ai-stewards (doc_* etc.), if configured
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
