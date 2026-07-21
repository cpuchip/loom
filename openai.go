package loom

// openai.go — an OpenAI-compatible /v1/chat/completions endpoint on the serve
// listener, so `loom serve` is a dispatchable MODEL PROVIDER, not only a
// harness dispatcher. pg-ai-stewards registers a `loom` provider pointing here;
// then ANY model alias (critic, review, or eventually all of them) can resolve
// to a harness-driven model — e.g. Claude sonnet via a Max subscription —
// instead of a paid per-token API. The request drives a fresh one-shot backend
// session and returns its reply in OpenAI shape. The MODEL NAME selects the
// backend (see shimBackendFor): claude-family names (the default) run claude,
// gpt*/codex*/sol/terra/luna run codex, copilot-* runs copilot, and
// "<backend>:<model>" pins explicitly.
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
// agent with no substrate tools, so a doc-construction critique/finalize
// stage cannot read or pool the draft. NOTE: a claude-routed session runs in a
// Linux container, so the config's server must be reachable FROM the container
// (a container-baked binary or an http endpoint via host.docker.internal), and
// any DSN must resolve there too. A codex/copilot/opencode-routed session runs
// on the HOST and resolves the config there instead (see hostMCPConfig).
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
//
// Home and workdir resolve INDEPENDENTLY: a host-run seat (codex/copilot/
// opencode) legitimately has a <role>-workdir with no <role>-claude-home —
// requiring a claude home just to unlock a codex seat's grounding was a
// claude-shaped assumption, dropped when serve-side skills landed.
func resolveModelHome(model string) (bareModel, home, workdir string) {
	base, role, found := strings.Cut(model, "#")
	if !found || role == "" || openaiHomeRoot == "" {
		return model, openaiClaudeHome, ""
	}
	home = openaiClaudeHome
	if h := filepath.Join(openaiHomeRoot, role+"-claude-home"); isDir(h) {
		home = h
	}
	if w := filepath.Join(openaiHomeRoot, role+"-workdir"); isDir(w) {
		workdir = w
	}
	return base, home, workdir
}

// resolveRoleSkills returns the role's authored-skills source directory
// (<root>/<role>-skills) for a "<model>#<role>" name — the serve-side twin of
// `loom run --skills`. "" when the model carries no role, no --openai-home-root
// is set, or the directory is missing (a config gap never fails a request).
func resolveRoleSkills(model string) string {
	_, role, found := strings.Cut(model, "#")
	if !found || role == "" || openaiHomeRoot == "" {
		return ""
	}
	if d := filepath.Join(openaiHomeRoot, role+"-skills"); isDir(d) {
		return d
	}
	return ""
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// shimBackendFor maps the shim's BARE model name (role already split off by
// resolveModelHome) to the backend that serves it, plus the model string that
// backend receives. Historically the shim was hardwired to claude — every
// /v1/* request became `claude --model <name>` no matter what the caller asked
// for — so "gpt-5.6-terra#capcom" ran a broken claude turn instead of a codex
// seat. This is the same select-a-backend step `loom run --agent` and the ws
// open frame already perform; the shim derives it from the model name because
// an OpenAI caller has no other selector.
//
// Rules, first match wins:
//
//  1. "<backend>:<model>" pins explicitly ("codex:gpt-5.6-terra",
//     "opencode:zen/glm-5.2", "claude:sonnet") — the escape hatch when a name
//     shape lies. Only shim-supported backends (claude/codex/copilot/opencode)
//     are recognized; agy is structurally limited (no stream-json) and local
//     needs its own model plumbing, so neither is routable here.
//  2. A bare backend name ("codex", "copilot", "opencode") runs that backend's
//     own default model.
//  3. "copilot-<model>" peels the prefix → copilot ("copilot-gpt-5" → gpt-5).
//  4. OpenAI-shaped names (gpt*, codex*) and the codex fleet nicknames
//     (sol/terra/luna) → codex, model passed through UNCHANGED — codex owns
//     resolution, so a nickname its catalog doesn't know errors clearly
//     instead of silently drifting to another model.
//  5. Everything else (sonnet, opus, haiku, claude-*, unknown, empty) keeps
//     the historical claude path byte-identical: default backend, model
//     passed through untouched.
//
// A "#role" suffix that survived resolveModelHome (a serve with no
// --openai-home-root) selects nothing on a non-claude backend — it is stripped
// from both the match and the model those backends receive. The claude
// fallthrough returns the ORIGINAL string untouched (legacy shape).
func shimBackendFor(model string) (agent, bareModel string) {
	head, _, _ := strings.Cut(model, "#")
	lower := strings.ToLower(head)
	if pre, rest, found := strings.Cut(head, ":"); found {
		switch p := strings.ToLower(pre); p {
		case "claude", "codex", "copilot", "opencode":
			return p, rest
		}
	}
	switch {
	case lower == "codex" || lower == "copilot" || lower == "opencode":
		return lower, ""
	case strings.HasPrefix(lower, "copilot-"):
		return "copilot", head[len("copilot-"):]
	case strings.HasPrefix(lower, "gpt") || strings.HasPrefix(lower, "codex") ||
		lower == "sol" || lower == "terra" || lower == "luna":
		return "codex", head
	}
	return "claude", model
}

// hostMCPConfig resolves the shim's --mcp-config for a backend that runs ON THE
// HOST (codex, copilot, opencode — no docker container, no /home/node). The
// mirror of effectiveMCPConfig: a container-anchored path
// (/home/node/.claude/<file>) names a file that physically lives in the role
// home's HOST directory, so map it there when it exists — else drop the hinge
// (toolless, never a crashed turn). A non-anchored path is already a host path
// and passes through untouched. NOTE the mapped file's CONTENT is the operator's
// contract: a config written for in-container consumption (container-baked
// binary paths, host.docker.internal URLs) may not resolve from the host.
func hostMCPConfig(cfg, home string) string {
	base, anchored := strings.CutPrefix(cfg, "/home/node/.claude/")
	if !anchored {
		return cfg // empty, or a host path already — pass through
	}
	if home == "" {
		return ""
	}
	p := filepath.Join(home, filepath.FromSlash(base))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
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

// effectiveMCPConfig decides whether a session actually receives the shim's
// --mcp-config. The path is resolved INSIDE the session container, where a
// home-anchored config (/home/node/.claude/<file>) exists only when that home is
// mounted AND actually contains the file. Handing claude a --mcp-config it cannot
// open makes it EXIT at startup ("Invalid MCP configuration: MCP config file not
// found: …"), which the caller sees as the opaque "stream ended before a result
// event (process exited?)". So for a home-anchored config, drop the hinge —
// degrade to a toolless-but-working turn — unless the file is really present in
// the mounted home. A bare "sonnet" (no home mounted) or a role home missing the
// file loses tools, never the turn; this mirrors resolveModelHome's rule that a
// config gap never fails a request, it just loses the specialization. A config
// that is NOT home-anchored (e.g. an image-baked binary path) is passed through
// untouched — loom cannot second-guess a path it does not own.
func effectiveMCPConfig(cfg, home string) string {
	base, anchored := strings.CutPrefix(cfg, "/home/node/.claude/")
	if !anchored {
		return cfg // empty, or an image-relative path loom can't verify — trust it
	}
	if home == "" {
		return "" // no home mount → the anchored path can't exist in the container
	}
	if _, err := os.Stat(filepath.Join(home, base)); err != nil {
		return "" // home mounted but the config file isn't in it → toolless, not a crash
	}
	return cfg
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

	// Role-aware environment: "sonnet#critic" -> sonnet + the critic home
	// (+ the critic workdir mounted ro as /work, if <root>/critic-workdir exists).
	bareModel, home, workdir := resolveModelHome(req.Model)

	// Model → backend: "gpt-5.6-terra#capcom" runs a codex seat, "copilot-gpt-5"
	// a copilot one; claude-family names keep the historical claude path
	// byte-identical (see shimBackendFor).
	agent, routedModel := shimBackendFor(bareModel)
	be, ok := s.backends[agent]
	if !ok {
		writeOpenAIErr(w, req.Stream, fmt.Errorf("no %q backend on this loom serve", agent))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), openaiTimeout)
	defer cancel()

	// #333: a substrate dispatch declares its session in the standard `user`
	// field — derive a per-session MCP config carrying it as a header, so the
	// session's doc drafts scope to the work item. Fallback = static config.
	// The wi-- derivation is CLAUDE-ONLY for now: it maps paths in and out of
	// the claude container, and the substrate's doc-construction seats are
	// claude seats; a non-claude wi-- dispatch runs on the static config and
	// merely loses the provenance header, never the request.
	mcpCfg := effectiveMCPConfig(openaiMCPConfig, home)
	if agent != "claude" {
		mcpCfg = hostMCPConfig(openaiMCPConfig, home)
	}
	if agent == "claude" && strings.HasPrefix(req.User, "wi--") {
		if derived, host := sessionMCPConfig(home, openaiMCPConfig, req.User); derived != "" {
			mcpCfg = derived
			defer os.Remove(host)
		}
	}

	// baseOpts is the per-turn session shape; only Resume varies (cold path) or is
	// unused (warm path — the live process IS the continuation, no --resume).
	baseOpts := SessionOpts{
		Model:     routedModel, // loom passes --model straight through
		Isolate:   true,        // claude: docker sandbox; codex/copilot: their NATIVE wall (workspace-write / allow-all-tools+path-verification)
		Workdir:   workdir,     // role context ("" = serve's own cwd, the historical default)
		MCPConfig: mcpCfg,      // hinge into pg-ai-stewards (doc_* etc.), if configured
	}
	if agent == "claude" {
		// The historical shape, unchanged: skip-permissions is safe INSIDE the
		// docker wall, and the role home mounts as the container's ~/.claude.
		baseOpts.SkipPermissions = true
		baseOpts.ClaudeHome = home
		baseOpts.WorkdirRO = workdir != ""
	}
	// Non-claude backends run on the HOST: SkipPermissions is deliberately NOT
	// set (it would strip the only wall — codex --dangerously-bypass…, copilot
	// --allow-all), ClaudeHome/WorkdirRO are container concepts they ignore, and
	// the role workdir is a host path they can cd into directly.
	//
	// Serve-side skills (#6 extended to the shim): a CLAUDE seat already gets
	// skills through its mounted role home (<role>-claude-home/skills/ IS the
	// container's ~/.claude/skills/) — nothing to deliver. A HOST-RUN seat reads
	// skills from its session workdir instead, so hand Open the role's authored
	// skills (<root>/<role>-skills) and let mirrorSkills place them in the
	// backend's own skill dir (.agents/ or .claude/) inside the role workdir —
	// the same author-once path `loom run --skills` uses. Only when a role
	// workdir exists: without one the seat's cwd is the serve process's own,
	// and loom does not scribble skill trees into the operator's cwd.
	if agent != "claude" && workdir != "" {
		baseOpts.SkillsDir = resolveRoleSkills(req.Model)
	}
	open := func(resume string) (Session, error) {
		o := baseOpts
		o.Resume = resume // sticky cold path: continue the living session by id
		return be.Open(ctx, o)
	}

	// Sticky conversations (user = "sticky:<name>", see openai_sticky.go): one
	// living session per (model,user). Cold path — the process is per-turn,
	// session state persists in the backend's own store (claude: the mounted
	// role home; codex/copilot/opencode: their native session stores), and
	// Resume carries the lineage — so sticky works across ALL routed backends.
	// Warm path (--openai-warm) — CLAUDE-ONLY: it exists to skip claude's
	// spawn+--resume floor; the per-turn backends spawn per turn regardless, so
	// a warm seat would hold nothing but a struct while stickyOverview reported
	// a live claude process that isn't there.
	var entry *stickyEntry
	resume := ""
	useWarm := false
	if key := stickyKeyFor(req.Model, req.User); key != "" {
		entry = stickyFor(key)
		entry.mu.Lock() // serialize turns within one conversation (held across the turn)
		defer entry.mu.Unlock()
		resume = entry.sessionID
		useWarm = openaiWarm && agent == "claude" && entry.canServeWarm()
	}
	// The session already holds prior context when we have a lineage to resume OR a
	// live warm seat; only then does the delta (messages after the last assistant)
	// suffice — otherwise replay the full transcript.
	hasContext := resume != "" || (entry != nil && entry.warm != nil)
	prompt := flattenMessages(req.Messages)
	if hasContext {
		if d, ok := flattenDelta(req.Messages); ok {
			prompt = d // the resumed/warm session already holds the rest
		}
	}

	// Streaming: forward assistant text SEGMENTS as SSE deltas while the turn
	// runs (claude emits one assistant event per message — text lands between
	// tool calls, so a voice caller can speak the first sentence while tools
	// are still working). Non-stream keeps the single-JSON reply.
	var sse *sseWriter
	var onEvent func(Event)
	if req.Stream {
		sse = newSSEWriter(w, req.Model)
		onEvent = func(ev Event) {
			if ev.Kind == EvAssistant && ev.Text != "" {
				// Segments are whole messages; join with a newline so the
				// caller's sentence splitter (TTS) hears a boundary instead
				// of "now.No" run-ons.
				sse.chunk(ev.Text + "\n")
			}
		}
	}

	runOnce := func(res string, p string) (Reply, error) {
		sess, err := open(res)
		if err != nil {
			return Reply{}, fmt.Errorf("open session: %w", err)
		}
		defer sess.Close()
		if onEvent != nil {
			return sess.SendStream(ctx, p, onEvent)
		}
		return sess.Send(ctx, p)
	}

	var reply Reply
	var err error
	if useWarm {
		// Warm path: run on (or first establish) the live seat. A crash tears the
		// seat down inside runStickyWarm and returns an error, which the cold
		// fallback below catches — a warm crash degrades to cold, never fails.
		reply, err = entry.runStickyWarm(be, baseOpts, resume, prompt, onEvent)
	} else {
		reply, err = runOnce(resume, prompt)
	}
	if err == nil && reply.Err != "" {
		err = fmt.Errorf("%s", reply.Err)
	}
	// Resume/crash fallback: the caller sends the FULL history every turn, so a dead
	// session (file gone, home swapped) OR a torn-down warm seat costs nothing —
	// drop the mapping and replay the whole transcript into a fresh COLD session,
	// once. Only when no streamed bytes are out yet (a mid-stream failure can't be
	// restarted).
	if err != nil && resume != "" && (sse == nil || !sse.started()) {
		entry.sessionID = ""
		reply, err = runOnce("", flattenMessages(req.Messages))
		if err == nil && reply.Err != "" {
			err = fmt.Errorf("%s", reply.Err)
		}
	}
	if err != nil {
		if sse != nil && sse.started() {
			sse.fail(err)
			return
		}
		writeOpenAIErr(w, req.Stream, fmt.Errorf("send: %w", err))
		return
	}

	if entry != nil && reply.SessionID != "" {
		// --resume may mint a new id that continues the transcript; always
		// follow the latest.
		entry.sessionID = reply.SessionID
		stickyTouch(entry)
	}

	if sse != nil {
		sse.finish(reply.Text)
	} else {
		writeOpenAIJSON(w, req.Model, reply.Text)
	}
}

// sseWriter emits OpenAI streaming chunks incrementally. finish() closes the
// stream; if no assistant segments were streamed (backend produced only a
// final result), it falls back to emitting the reply text as one chunk so the
// caller never receives an empty stream.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	sent    bool
	headers bool
}

func newSSEWriter(w http.ResponseWriter, model string) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: f, model: model}
}

func (s *sseWriter) started() bool { return s.sent }

func (s *sseWriter) emit(v any) {
	if !s.headers {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.headers = true
		s.emit(map[string]any{"id": "chatcmpl-loom", "object": "chat.completion.chunk", "model": s.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}}})
	}
	bs, _ := json.Marshal(v)
	fmt.Fprintf(s.w, "data: %s\n\n", bs)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *sseWriter) chunk(text string) {
	s.sent = true
	s.emit(map[string]any{"id": "chatcmpl-loom", "object": "chat.completion.chunk", "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}})
}

func (s *sseWriter) finish(replyText string) {
	if !s.sent && replyText != "" {
		s.chunk(replyText)
	}
	s.emit(map[string]any{"id": "chatcmpl-loom", "object": "chat.completion.chunk", "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *sseWriter) fail(err error) {
	s.emit(map[string]any{"error": map[string]any{"message": err.Error()}})
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
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
