package loom

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// AgyBackend drives Google Antigravity's `agy` (Gemini). Unlike Claude Code, agy
// has NO stream-json mode — each turn is a one-shot `agy -p` with two known bugs:
//
//  1. it HANGS waiting on stdin EOF in a non-TTY → we feed an empty stdin.
//  2. it DROPS the response from stdout (exit 0, empty pipe) → we recover it from
//     the on-disk transcript (newest …/transcript.jsonl, last PLANNER_RESPONSE).
//
// Multi-turn is via `--conversation <id>` resume (a fresh process per turn). This
// backend is EXPERIMENTAL: the recovery is timing-sensitive (newest-transcript
// heuristic) and it spends the Google subscription, so the smoke test does not
// exercise it. The point of including it is to prove loom abstracts heterogeneous
// CLIs — claude (persistent stream-json) and agy (one-shot + transcript) behind
// one Session interface.
type AgyBackend struct {
	Bin      string // agy executable
	BrainDir string // ~/.gemini/antigravity-cli/brain
}

// DefaultAgyBackend resolves the usual Windows install path + brain dir.
func DefaultAgyBackend() AgyBackend {
	home, _ := os.UserHomeDir()
	bin := "agy"
	if p := filepath.Join(home, "AppData", "Local", "agy", "bin", "agy.exe"); fileExists(p) {
		bin = p
	}
	return AgyBackend{Bin: bin, BrainDir: filepath.Join(home, ".gemini", "antigravity-cli", "brain")}
}

func (b AgyBackend) Name() string { return "agy" }

func (b AgyBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
	return &agySession{b: b, opts: opts}, nil
}

type agySession struct {
	b      AgyBackend
	opts   SessionOpts
	mu     sync.Mutex
	convID string
}

func (s *agySession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}

func (s *agySession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	args := []string{"-p", prompt, "--dangerously-skip-permissions"}
	if s.convID != "" {
		args = append(args, "--conversation", s.convID)
	}
	cmd := exec.CommandContext(ctx, s.b.Bin, args...)
	if s.opts.Workdir != "" {
		cmd.Dir = s.opts.Workdir
	}
	cmd.Stdin = bytes.NewReader(nil) // closed stdin → avoid the EOF hang (bug #1)
	_ = cmd.Run()                    // stdout is dropped by agy (bug #2); ignore it

	text, conv := s.b.recoverLatest() // recover from the transcript
	if s.convID == "" && conv != "" {
		s.convID = conv
	}
	emit(onEvent, Event{Kind: EvAssistant, Backend: "agy", Text: text})
	emit(onEvent, Event{Kind: EvResult, Backend: "agy", Text: text})
	r := Reply{Backend: "agy", Text: text, SessionID: s.convID}
	if strings.TrimSpace(text) == "" {
		r.Err = "agy: empty transcript recovery (response may not have flushed; or no transcript under BrainDir)"
	}
	return r, nil
}

func (s *agySession) SessionID() string { return s.convID }
func (s *agySession) Close() error      { return nil }

// recoverLatest reads the newest transcript.jsonl under BrainDir and returns the
// last non-empty PLANNER_RESPONSE content + its conversation id (the dir name).
func (b AgyBackend) recoverLatest() (text, convID string) {
	var newest string
	var newestMod int64 = -1
	_ = filepath.WalkDir(b.BrainDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "transcript.jsonl" {
			return nil
		}
		if info, e := d.Info(); e == nil && info.ModTime().UnixNano() > newestMod {
			newestMod, newest = info.ModTime().UnixNano(), p
		}
		return nil
	})
	if newest == "" {
		return "", ""
	}
	convID = convIDFromPath(newest, b.BrainDir)
	data, err := os.ReadFile(newest)
	if err != nil {
		return "", convID
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"PLANNER_RESPONSE"`) {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "PLANNER_RESPONSE" && strings.TrimSpace(ev.Content) != "" {
			text = ev.Content // keep the LAST non-empty response
		}
	}
	return text, convID
}

func convIDFromPath(p, brainDir string) string {
	rel, err := filepath.Rel(brainDir, p)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
