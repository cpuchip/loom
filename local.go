package loom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// LocalBackend drives a local OpenAI-compatible endpoint (llama-chip's :8090
// router, LM Studio, vLLM, …). It's the SIMPLEST backend: no process to spawn, no
// stdio protocol, no transcript-scrape — just a stateless POST /v1/chat/completions.
// loom keeps the message history per session for multi-turn (the OpenAI API is
// stateless, so you resend the history). This makes `panel` a real cloud+local
// council — e.g. a fast local doer + claude critic.
type LocalBackend struct {
	BaseURL string // default http://localhost:8090/v1 (override via LOOM_LOCAL_URL)
	Model   string // default model when SessionOpts.Model is empty (else asks /v1/models)
	APIKey  string // optional; local routers usually ignore it
}

func DefaultLocalBackend() LocalBackend {
	base := os.Getenv("LOOM_LOCAL_URL")
	if base == "" {
		base = "http://localhost:8090/v1"
	}
	return LocalBackend{
		BaseURL: strings.TrimRight(base, "/"),
		Model:   os.Getenv("LOOM_LOCAL_MODEL"),
		APIKey:  os.Getenv("LOOM_LOCAL_KEY"),
	}
}

func (b LocalBackend) Name() string { return "local" }

func (b LocalBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
	model := opts.Model
	if model == "" {
		model = b.Model
	}
	return &localSession{b: b, model: model}, nil
}

type localSession struct {
	b       LocalBackend
	model   string
	mu      sync.Mutex
	history []oaiMsg
}

type oaiMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiReq struct {
	Model     string   `json:"model"`
	Messages  []oaiMsg `json:"messages"`
	MaxTokens int      `json:"max_tokens,omitempty"`
}

type oaiResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// some reasoning models (qwen3.x on LM Studio) put the answer in
			// content and the chain-of-thought in reasoning_content; if content
			// is empty we fall back to it.
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (s *localSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}

func (s *localSession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	model := s.model
	if model == "" { // no model given — ask the endpoint what it serves
		if m := s.b.firstModel(ctx); m != "" {
			model, s.model = m, m
		} else {
			return Reply{Backend: "local", Err: "no model set (use --model, LOOM_LOCAL_MODEL, or a reachable /v1/models)"},
				fmt.Errorf("local: no model and /v1/models gave none")
		}
	}

	// Build the request history WITHOUT mutating s.history yet — a failed turn must
	// not leave a dangling user message that poisons the next turn (two consecutive
	// role:"user" messages, which strict OpenAI servers reject). Commit on success.
	msgs := append(append([]oaiMsg(nil), s.history...), oaiMsg{Role: "user", Content: prompt})
	// ≥2000 tokens so reasoning models don't truncate to empty content (see the
	// LM Studio qwen3 thinking-budget gotcha).
	body, err := json.Marshal(oaiReq{Model: model, Messages: msgs, MaxTokens: 4096})
	if err != nil {
		return Reply{Backend: "local", Err: err.Error()}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.b.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Reply{Backend: "local", Err: err.Error()}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.b.APIKey)
	}

	resp, err := (&http.Client{Timeout: 300 * time.Second}).Do(req)
	if err != nil {
		return Reply{Backend: "local", Err: err.Error()}, err
	}
	defer resp.Body.Close()

	var r oaiResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Reply{Backend: "local", Err: "decode: " + err.Error()}, err
	}
	if r.Error != nil {
		return Reply{Backend: "local", Err: r.Error.Message}, fmt.Errorf("local: %s", r.Error.Message)
	}
	if len(r.Choices) == 0 {
		return Reply{Backend: "local", Err: "no choices in response"}, fmt.Errorf("local: empty response")
	}
	choice := r.Choices[0].Message
	text := stripThink(choice.Content)
	if strings.TrimSpace(text) == "" {
		text = stripThink(choice.ReasoningContent) // qwen-style fallback
	}
	if strings.TrimSpace(text) == "" {
		// empty answer — don't commit it (an empty assistant turn poisons context);
		// surface it as an error so the turn fails cleanly instead.
		return Reply{Backend: "local", Err: "empty response (no content / reasoning_content)"},
			fmt.Errorf("local: empty response")
	}
	if strings.TrimSpace(choice.ReasoningContent) != "" && strings.TrimSpace(choice.Content) != "" {
		emit(onEvent, Event{Kind: EvThinking, Backend: "local", Text: choice.ReasoningContent})
	}
	emit(onEvent, Event{Kind: EvAssistant, Backend: "local", Text: text})
	// commit BOTH turns only now that the turn has succeeded
	s.history = append(msgs, oaiMsg{Role: "assistant", Content: text})
	emit(onEvent, Event{Kind: EvResult, Backend: "local", Text: text})
	return Reply{Backend: "local", Text: text, SessionID: "local:" + model, Turns: 1, CostUSD: 0}, nil
}

func (s *localSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return "local:" + s.model
}
func (s *localSession) Close() error      { return nil }

// firstModel returns the first id from /v1/models (used when no model is set).
func (b LocalBackend) firstModel(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, "GET", b.BaseURL+"/models", nil)
	if err != nil {
		return ""
	}
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var ml struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&ml) == nil && len(ml.Data) > 0 {
		return ml.Data[0].ID
	}
	return ""
}

// stripThink removes <think>…</think> chain-of-thought that reasoning models
// (qwen3.x, deepseek-r1, …) emit inline in content. It handles BOTH the matched
// <think>…</think> pairs AND the common llama.cpp/vLLM serving case where the
// OPENING <think> is seeded into the prompt and not echoed — so the completion is
// reasoning → </think> → answer with an orphan closing tag. (Models that narrate
// reasoning with no tags at all still narrate; for those, use a server that
// separates reasoning_content, or a non-reasoning model.)
func stripThink(s string) string {
	// matched pairs first
	for {
		i := strings.Index(s, "<think>")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "</think>")
		if j < 0 {
			break
		}
		s = s[:i] + s[i+j+len("</think>"):]
	}
	// any remaining </think> is an orphan (its <think> was prompt-seeded) — strip
	// everything up to and including it (the reasoning prefix).
	if i := strings.Index(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	return strings.TrimSpace(s)
}
