package loom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// RaceContender identifies one local agent/model combination in a race.
// Backend is deliberately injected here so Race remains testable without invoking live CLIs.
type RaceContender struct {
	Agent   string
	Model   string
	Backend Backend
}

// RaceContenderResult records one contender's final outcome. Dir is intentionally kept on
// the winner only: the winning tree is the deliverable, while the others are diagnostics.
type RaceContenderResult struct {
	Agent     string  `json:"agent"`
	Model     string  `json:"model"`
	Status    string  `json:"status"`
	OracleRC  int     `json:"oracle_rc"`
	DurationS float64 `json:"duration_s"`
}

// RaceWinner is the contender that first satisfied the oracle.
type RaceWinner struct {
	Agent     string  `json:"agent"`
	Model     string  `json:"model"`
	Dir       string  `json:"dir"`
	DurationS float64 `json:"duration_s"`
}

// RaceResult is the final race report. Winner is nil when no contender passed.
type RaceResult struct {
	Winner     *RaceWinner           `json:"winner"`
	Contenders []RaceContenderResult `json:"contenders"`
	Status     string                `json:"status"`
	CostUSD    float64               `json:"cost_usd"`
}

// RaceObserver receives progress as contenders start, finish, and the oracle judges them.
// The CLI uses it for narration while the engine stays independent of terminal output.
type RaceObserver struct {
	Start   func(contender RaceContender, dir string)
	Finish  func(contender RaceContender, result RaceContenderResult)
	Verdict func(contender RaceContender, rc int, passed bool)
}

// RaceConfig configures a one-turn race. Dir is copied per contender; no Dir means each
// contender starts in an empty temporary directory. Timeout covers the whole race.
type RaceConfig struct {
	Contenders []RaceContender
	Opts       SessionOpts
	Prompt     string
	Oracle     string
	Dir        string
	Timeout    time.Duration
	Observer   RaceObserver
}

const (
	raceStatusWon      = "won"
	raceStatusNoWinner = "no_winner"
	raceStatusTimeout  = "timeout"
)

// ParseRaceContenders parses agent[:model] entries without a regexp so the accepted shape
// stays obvious alongside the CLI's backend lookup. Backend names themselves are checked by
// the caller, because this parser is also useful to callers with injected backends.
func ParseRaceContenders(list string) ([]RaceContender, error) {
	var contenders []RaceContender
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, fmt.Errorf("race: invalid contender list")
		}
		parts := strings.Split(raw, ":")
		if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
			return nil, fmt.Errorf("race: invalid contender %q (want agent or agent:model)", raw)
		}
		for _, part := range parts {
			if strings.TrimSpace(part) != part || strings.ContainsAny(part, " \t\r\n") {
				return nil, fmt.Errorf("race: invalid contender %q (want agent or agent:model)", raw)
			}
		}
		c := RaceContender{Agent: parts[0]}
		if len(parts) == 2 {
			c.Model = parts[1]
		}
		contenders = append(contenders, c)
	}
	return contenders, nil
}

type raceCompletion struct {
	index  int
	result RaceContenderResult
	dir    string
	cost   float64
	passed bool
}

// Race starts every contender at once, then judges each completed tree. The first oracle
// pass cancels the shared context before waiting for cleanup: this makes the winner's tree
// stable while ensuring losing sessions receive cancellation and close promptly.
func Race(ctx context.Context, cfg RaceConfig) (RaceResult, error) {
	if strings.TrimSpace(cfg.Oracle) == "" {
		return RaceResult{}, fmt.Errorf("race: -oracle is required (a race needs a deterministic judge)")
	}
	if len(cfg.Contenders) == 0 {
		return RaceResult{}, fmt.Errorf("race: need at least one contender")
	}
	for _, c := range cfg.Contenders {
		if c.Agent == "" || c.Backend == nil {
			return RaceResult{}, fmt.Errorf("race: contender %q has no backend", c.Agent)
		}
	}

	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cfg.Timeout > 0 {
		var stop context.CancelFunc
		raceCtx, stop = context.WithTimeout(raceCtx, cfg.Timeout)
		defer stop()
	}

	res := RaceResult{Status: raceStatusNoWinner, Contenders: make([]RaceContenderResult, len(cfg.Contenders))}
	completed := make(chan raceCompletion, len(cfg.Contenders))
	var wg sync.WaitGroup
	for i, contender := range cfg.Contenders {
		wg.Add(1)
		go func(i int, contender RaceContender) {
			defer wg.Done()
			completed <- runRaceContender(raceCtx, cfg, i, contender)
		}(i, contender)
	}

	// Each worker sends exactly one completion, even after cancellation. Waiting for all of
	// them means Race never returns while a losing session still owns its working process.
	for range cfg.Contenders {
		completion := <-completed
		res.Contenders[completion.index] = completion.result
		res.CostUSD += completion.cost
		if cfg.Observer.Finish != nil {
			cfg.Observer.Finish(cfg.Contenders[completion.index], completion.result)
		}
		if completion.passed && res.Winner == nil {
			res.Winner = &RaceWinner{
				Agent: completion.result.Agent, Model: completion.result.Model,
				Dir: completion.dir, DurationS: completion.result.DurationS,
			}
			res.Status = raceStatusWon
			cancel()
		}
	}
	wg.Wait()
	if res.Winner == nil && errors.Is(raceCtx.Err(), context.DeadlineExceeded) {
		res.Status = raceStatusTimeout
	}
	return res, nil
}

func runRaceContender(ctx context.Context, cfg RaceConfig, index int, contender RaceContender) raceCompletion {
	started := time.Now()
	result := RaceContenderResult{Agent: contender.Agent, Model: contender.Model, OracleRC: -1}
	dir, err := raceWorkdir(cfg.Dir)
	if err != nil {
		result.Status = "error"
		result.DurationS = time.Since(started).Seconds()
		return raceCompletion{index: index, result: result}
	}
	if cfg.Observer.Start != nil {
		cfg.Observer.Start(contender, dir)
	}

	opts := cfg.Opts
	opts.Workdir = dir
	// An explicit contender model wins, while an agent-only contender retains the caller's
	// default model. This makes the compact agent[:model] syntax additive to run's flags.
	if contender.Model != "" {
		opts.Model = contender.Model
	}
	// A race compares fresh, independent attempts; sharing a resumed conversation would
	// leak work between contenders even though their filesystem trees are isolated.
	opts.Resume = ""
	sess, err := contender.Backend.Open(ctx, opts)
	if err != nil {
		result.Status = raceContextStatus(ctx, "error")
		result.DurationS = time.Since(started).Seconds()
		return raceCompletion{index: index, result: result}
	}
	defer sess.Close()
	reply, err := sess.Send(ctx, cfg.Prompt)
	result.DurationS = time.Since(started).Seconds()
	if err != nil || reply.Err != "" {
		result.Status = raceContextStatus(ctx, "error")
		return raceCompletion{index: index, result: result, cost: reply.CostUSD}
	}
	if ctx.Err() != nil {
		result.Status = "cancelled"
		return raceCompletion{index: index, result: result, cost: reply.CostUSD}
	}

	rc, oracleErr := runOracle(ctx, cfg.Oracle, dir)
	result.OracleRC = rc
	result.DurationS = time.Since(started).Seconds()
	if ctx.Err() != nil {
		result.Status = "cancelled"
		return raceCompletion{index: index, result: result, cost: reply.CostUSD}
	}
	passed := oracleErr == nil && rc == 0
	if cfg.Observer.Verdict != nil {
		cfg.Observer.Verdict(contender, rc, passed)
	}
	if passed {
		result.Status = "won"
		return raceCompletion{index: index, result: result, dir: dir, cost: reply.CostUSD, passed: true}
	}
	if oracleErr != nil && rc == -1 {
		result.Status = "error"
	} else {
		result.Status = "oracle_failed"
	}
	return raceCompletion{index: index, result: result, cost: reply.CostUSD}
}

func raceContextStatus(ctx context.Context, otherwise string) string {
	if ctx.Err() != nil {
		return "cancelled"
	}
	return otherwise
}

// raceWorkdir deliberately copies rather than cloning: the input can be any directory, and
// .git is skipped so each contender receives the task files, not the caller's repository state.
func raceWorkdir(source string) (string, error) {
	dir, err := os.MkdirTemp("", "loom-race-")
	if err != nil {
		return "", fmt.Errorf("race: make workdir: %w", err)
	}
	if source == "" {
		return dir, nil
	}
	if err := copyRaceTree(source, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func copyRaceTree(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("race: inspect source dir %q: %w", source, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("race: source %q is not a directory", source)
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, entry.Type().Perm())
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		return copyRaceFile(path, target, entry.Type())
	})
}

func copyRaceFile(source, destination string, mode fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func runOracle(ctx context.Context, command, dir string) (int, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), err
	}
	return -1, err
}
