package loom

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

type raceTestBackend struct {
	name string
	send func(context.Context, string, SessionOpts) (Reply, error)

	mu   sync.Mutex
	opts []SessionOpts
}

func (b *raceTestBackend) Name() string { return b.name }
func (b *raceTestBackend) Open(_ context.Context, opts SessionOpts) (Session, error) {
	b.mu.Lock()
	b.opts = append(b.opts, opts)
	b.mu.Unlock()
	return &raceTestSession{send: func(ctx context.Context, prompt string) (Reply, error) {
		return b.send(ctx, prompt, opts)
	}}, nil
}
func (b *raceTestBackend) workdir(t *testing.T) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.opts) != 1 {
		t.Fatalf("opens = %d, want 1", len(b.opts))
	}
	return b.opts[0].Workdir
}

type raceTestSession struct {
	send   func(context.Context, string) (Reply, error)
	closed bool
}

func (s *raceTestSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.send(ctx, prompt)
}
func (s *raceTestSession) SendStream(ctx context.Context, prompt string, _ func(Event)) (Reply, error) {
	return s.Send(ctx, prompt)
}
func (s *raceTestSession) SessionID() string { return "race-test" }
func (s *raceTestSession) Close() error      { s.closed = true; return nil }

func raceExit(code int) string {
	if runtime.GOOS == "windows" {
		return "exit /B " + string(rune('0'+code))
	}
	return "exit " + string(rune('0'+code))
}

func TestRaceFirstPassWinsAndCancelsOthers(t *testing.T) {
	cancelled := make(chan struct{})
	fast := &raceTestBackend{name: "fast", send: func(_ context.Context, _ string, opts SessionOpts) (Reply, error) {
		return Reply{CostUSD: 0.25}, os.WriteFile(filepath.Join(opts.Workdir, "pass"), []byte("yes"), 0o644)
	}}
	slow := &raceTestBackend{name: "slow", send: func(ctx context.Context, _ string, _ SessionOpts) (Reply, error) {
		<-ctx.Done()
		close(cancelled)
		return Reply{}, ctx.Err()
	}}
	res, err := Race(context.Background(), RaceConfig{
		Contenders: []RaceContender{{Agent: "fast", Backend: fast}, {Agent: "slow", Backend: slow}},
		Prompt:     "build", Oracle: fileOracle("pass"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != raceStatusWon || res.Winner == nil || res.Winner.Agent != "fast" {
		t.Fatalf("result = %+v, want fast winner", res)
	}
	if res.Contenders[0].Status != "won" || res.Contenders[1].Status != "cancelled" {
		t.Fatalf("statuses = %+v", res.Contenders)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("slow contender never observed cancellation")
	}
	if res.CostUSD != 0.25 {
		t.Errorf("cost = %v, want .25", res.CostUSD)
	}
}

func TestRaceOracleFailureLetsSlowerPasserWin(t *testing.T) {
	fast := &raceTestBackend{name: "fast", send: func(_ context.Context, _ string, _ SessionOpts) (Reply, error) { return Reply{}, nil }}
	slow := &raceTestBackend{name: "slow", send: func(_ context.Context, _ string, opts SessionOpts) (Reply, error) {
		time.Sleep(30 * time.Millisecond)
		return Reply{}, os.WriteFile(filepath.Join(opts.Workdir, "pass"), []byte("yes"), 0o644)
	}}
	res, err := Race(context.Background(), RaceConfig{
		Contenders: []RaceContender{{Agent: "fast", Backend: fast}, {Agent: "slow", Backend: slow}},
		Prompt:     "build", Oracle: fileOracle("pass"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Winner == nil || res.Winner.Agent != "slow" || res.Contenders[0].Status != "oracle_failed" || res.Contenders[1].Status != "won" {
		t.Fatalf("result = %+v", res)
	}
	if res.Contenders[0].OracleRC == 0 {
		t.Fatal("fast oracle unexpectedly passed")
	}
}

func TestRaceNoWinnerAndTimeout(t *testing.T) {
	t.Run("no winner", func(t *testing.T) {
		b := &raceTestBackend{name: "nope", send: func(context.Context, string, SessionOpts) (Reply, error) { return Reply{}, nil }}
		res, err := Race(context.Background(), RaceConfig{Contenders: []RaceContender{{Agent: "nope", Backend: b}}, Prompt: "build", Oracle: raceExit(1)})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != raceStatusNoWinner || res.Winner != nil || res.Contenders[0].Status != "oracle_failed" {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		b := &raceTestBackend{name: "slow", send: func(ctx context.Context, _ string, _ SessionOpts) (Reply, error) {
			<-ctx.Done()
			return Reply{}, ctx.Err()
		}}
		res, err := Race(context.Background(), RaceConfig{Contenders: []RaceContender{{Agent: "slow", Backend: b}}, Prompt: "build", Oracle: raceExit(0), Timeout: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != raceStatusTimeout || res.Contenders[0].Status != "cancelled" {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestParseRaceContenders(t *testing.T) {
	got, err := ParseRaceContenders("codex, claude:sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Agent != "codex" || got[0].Model != "" || got[1].Agent != "claude" || got[1].Model != "sonnet" {
		t.Fatalf("parsed = %+v", got)
	}
	for _, in := range []string{"", "codex:", ":sonnet", "codex:a:b", "codex,", "co dex"} {
		if _, err := ParseRaceContenders(in); err == nil {
			t.Errorf("ParseRaceContenders(%q) accepted garbage", in)
		}
	}
}

func TestRaceRequiresOracle(t *testing.T) {
	_, err := Race(context.Background(), RaceConfig{Contenders: []RaceContender{{Agent: "fake", Backend: &raceTestBackend{name: "fake", send: func(context.Context, string, SessionOpts) (Reply, error) { return Reply{}, nil }}}}})
	if err == nil || !errors.Is(err, err) || err.Error() != "race: -oracle is required (a race needs a deterministic judge)" {
		t.Fatalf("error = %v", err)
	}
}

func TestRaceCopiesIsolatedWorkdirsWithoutGit(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "seed.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".git", "config"), []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	check := func(_ context.Context, _ string, opts SessionOpts) (Reply, error) {
		if _, err := os.Stat(filepath.Join(opts.Workdir, "seed.txt")); err != nil {
			return Reply{}, err
		}
		if _, err := os.Stat(filepath.Join(opts.Workdir, ".git")); !os.IsNotExist(err) {
			return Reply{}, errors.New("copied .git")
		}
		return Reply{}, nil
	}
	a := &raceTestBackend{name: "a", send: check}
	b := &raceTestBackend{name: "b", send: check}
	res, err := Race(context.Background(), RaceConfig{Contenders: []RaceContender{{Agent: "a", Backend: a}, {Agent: "b", Backend: b}}, Prompt: "build", Oracle: raceExit(1), Dir: source})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != raceStatusNoWinner {
		t.Fatalf("status = %q", res.Status)
	}
	if a.workdir(t) == b.workdir(t) {
		t.Fatal("contenders shared a workdir")
	}
}

func fileOracle(name string) string {
	if runtime.GOOS == "windows" {
		return "if exist " + name + " (exit /B 0) else (exit /B 1)"
	}
	return "test -f " + name
}
