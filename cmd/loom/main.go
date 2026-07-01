// Command loom drives coding-agent CLIs (Claude Code, agy/Gemini) as workers.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"

	"github.com/cpuchip/loom"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "chat":
		err = cmdChat(os.Args[2:])
	case "panel":
		err = cmdPanel(os.Args[2:])
	case "review":
		err = cmdReview(os.Args[2:])
	case "agents":
		for name := range loom.Backends() {
			fmt.Println(name)
		}
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "loom: "+err.Error())
		os.Exit(1)
	}
}

func pickBackend(name string) (loom.Backend, error) {
	b, ok := loom.Backends()[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q (try: loom agents)", name)
	}
	return b, nil
}

// run: one-shot prompt → single reply.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	agent := fs.String("agent", "claude", "backend (loom agents)")
	model := fs.String("model", "", "model override")
	dir := fs.String("dir", "", "working dir")
	events := fs.Bool("events", false, "stream tool calls + thinking to stderr")
	isolate := fs.Bool("isolate", false, "run claude in a docker sandbox (host walled off)")
	remote := fs.String("remote", "", "run claude on a remote box over ssh (e.g. cpuchip@host)")
	_ = fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return fmt.Errorf("run: need a prompt")
	}
	b, err := pickBackend(*agent)
	if err != nil {
		return err
	}
	sess, err := b.Open(context.Background(), loom.SessionOpts{Workdir: *dir, Model: *model, Isolate: *isolate, Remote: *remote})
	if err != nil {
		return err
	}
	defer sess.Close()
	r, err := sendTurn(sess, prompt, *events)
	if err != nil {
		return err
	}
	fmt.Println(r.Text)
	if r.CostUSD > 0 {
		fmt.Fprintf(os.Stderr, "[%s $%.4f]\n", r.Backend, r.CostUSD)
	}
	return nil
}

// chat: persistent multi-turn session — one message per stdin line.
func cmdChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	agent := fs.String("agent", "claude", "backend (loom agents)")
	model := fs.String("model", "", "model override")
	dir := fs.String("dir", "", "working dir")
	events := fs.Bool("events", false, "stream tool calls + thinking to stderr")
	isolate := fs.Bool("isolate", false, "run claude in a docker sandbox (host walled off)")
	remote := fs.String("remote", "", "run claude on a remote box over ssh (e.g. cpuchip@host)")
	_ = fs.Parse(args)
	b, err := pickBackend(*agent)
	if err != nil {
		return err
	}
	sess, err := b.Open(context.Background(), loom.SessionOpts{Workdir: *dir, Model: *model, Isolate: *isolate, Remote: *remote})
	if err != nil {
		return err
	}
	defer sess.Close()
	fmt.Fprintf(os.Stderr, "loom chat — %s — one message per line, Ctrl-D to end\n", *agent)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		r, err := sendTurn(sess, line, *events)
		if err != nil {
			return err
		}
		fmt.Println(r.Text)
		if r.CostUSD > 0 {
			fmt.Fprintf(os.Stderr, "[%s $%.4f this turn]\n", r.Backend, r.CostUSD)
		}
	}
	return in.Err()
}

// panel: fan one prompt across several agents (the council pattern).
func cmdPanel(args []string) error {
	fs := flag.NewFlagSet("panel", flag.ExitOnError)
	agents := fs.String("agents", "claude", "comma-separated backends")
	dir := fs.String("dir", "", "working dir")
	model := fs.String("model", "", "model override")
	isolate := fs.Bool("isolate", false, "run claude in a docker sandbox (host walled off)")
	remote := fs.String("remote", "", "run claude on a remote box over ssh (e.g. cpuchip@host)")
	_ = fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return fmt.Errorf("panel: need a prompt")
	}
	var backends []loom.Backend
	for _, name := range strings.Split(*agents, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		b, err := pickBackend(name)
		if err != nil {
			return err
		}
		backends = append(backends, b)
	}
	replies := loom.Panel(context.Background(), backends, loom.SessionOpts{Workdir: *dir, Model: *model, Isolate: *isolate, Remote: *remote}, prompt)
	for _, r := range replies {
		fmt.Printf("\n=== %s ===\n", r.Backend)
		if r.Err != "" {
			fmt.Printf("ERROR: %s\n", r.Err)
			continue
		}
		fmt.Println(r.Text)
		if r.CostUSD > 0 {
			fmt.Printf("[$%.4f]\n", r.CostUSD)
		}
	}
	return nil
}

// review: load a diff (or files) and fan a code-review across one or more agents.
func cmdReview(args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	agents := fs.String("agents", "claude", "comma-separated backends")
	dir := fs.String("dir", "", "repo dir for --diff (default: cwd)")
	diff := fs.String("diff", "", "review `git diff <args>` (e.g. HEAD, main...HEAD, \"HEAD~1 HEAD\")")
	model := fs.String("model", "", "model override")
	maxChars := fs.Int("max", 40000, "cap on review content chars")
	events := fs.Bool("events", false, "stream tool calls/thinking to stderr")
	isolate := fs.Bool("isolate", false, "run claude in a docker sandbox (host walled off)")
	remote := fs.String("remote", "", "run claude on a remote box over ssh (e.g. cpuchip@host)")
	_ = fs.Parse(args)

	content, label, err := gatherReview(*dir, *diff, fs.Args(), *maxChars)
	if err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("review: nothing to review (no files given, and the diff is empty)")
	}
	prompt := reviewPrompt(label, content)
	opts := loom.SessionOpts{Workdir: *dir, Model: *model, Isolate: *isolate, Remote: *remote}

	backends, err := backendsFromList(*agents)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "reviewing %s (%d chars) with: %s\n", label, len(content), *agents)

	if len(backends) == 1 {
		sess, err := backends[0].Open(context.Background(), opts)
		if err != nil {
			return err
		}
		defer sess.Close()
		r, err := sendTurn(sess, prompt, *events)
		if err != nil {
			return err
		}
		fmt.Println(r.Text)
		if r.CostUSD > 0 {
			fmt.Fprintf(os.Stderr, "[%s $%.4f]\n", r.Backend, r.CostUSD)
		}
		return nil
	}
	for _, rep := range loom.Panel(context.Background(), backends, opts, prompt) {
		fmt.Printf("\n=== %s ===\n", rep.Backend)
		if rep.Err != "" {
			fmt.Printf("ERROR: %s\n", rep.Err)
			continue
		}
		fmt.Println(rep.Text)
		if rep.CostUSD > 0 {
			fmt.Printf("[$%.4f]\n", rep.CostUSD)
		}
	}
	return nil
}

func backendsFromList(list string) ([]loom.Backend, error) {
	var out []loom.Backend
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		b, err := pickBackend(name)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no agents given")
	}
	return out, nil
}

// gatherReview collects what to review: the named files, else a git diff.
func gatherReview(dir, diff string, files []string, maxChars int) (content, label string, err error) {
	if len(files) > 0 {
		var sb strings.Builder
		for _, f := range files {
			b, e := os.ReadFile(f)
			if e != nil {
				return "", "", fmt.Errorf("read %s: %w", f, e)
			}
			fmt.Fprintf(&sb, "===== %s =====\n%s\n\n", f, b)
		}
		return clip(sb.String(), maxChars), fmt.Sprintf("%d file(s)", len(files)), nil
	}
	base := []string{}
	if dir != "" {
		base = append(base, "-C", dir)
	}
	if diff != "" {
		out, e := runGit(append(append([]string{}, base...), append([]string{"diff"}, strings.Fields(diff)...)...))
		if e != nil {
			return "", "", e
		}
		return clip(out, maxChars), "git diff " + diff, nil
	}
	// default: uncommitted changes vs HEAD; if none, the latest commit
	if out, _ := runGit(append(append([]string{}, base...), "diff", "HEAD")); strings.TrimSpace(out) != "" {
		return clip(out, maxChars), "the working-tree diff (vs HEAD)", nil
	}
	out, e := runGit(append(append([]string{}, base...), "show", "HEAD"))
	if e != nil {
		return "", "", e
	}
	return clip(out, maxChars), "the latest commit (HEAD)", nil
}

func runGit(args []string) (string, error) {
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n // back up to a UTF-8 rune boundary so we don't split a multibyte char
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…(truncated)…\n"
}

func reviewPrompt(label, content string) string {
	return "You are a senior code reviewer. Review the following " + label +
		" for CORRECTNESS bugs, security risks, and real defects — not style or formatting. " +
		"List the most important findings (most severe first), each with the location if visible " +
		"and a one-line why. If it looks correct, say so plainly. Be specific and concise.\n\n" +
		content
}

// sendTurn runs one turn, optionally streaming the agent's tool calls + thinking
// to stderr (the work) while the final answer is returned in the Reply.
func sendTurn(sess loom.Session, prompt string, showEvents bool) (loom.Reply, error) {
	if showEvents {
		return sess.SendStream(context.Background(), prompt, eventPrinter())
	}
	return sess.Send(context.Background(), prompt)
}

func eventPrinter() func(loom.Event) {
	return func(ev loom.Event) {
		switch ev.Kind {
		case loom.EvToolCall:
			fmt.Fprintf(os.Stderr, "  → %s\n", ev.Tool)
		case loom.EvToolResult:
			fmt.Fprintln(os.Stderr, "  ← (tool result)")
		case loom.EvThinking:
			fmt.Fprintf(os.Stderr, "  · %s\n", oneLine(ev.Text, 100))
			// EvAssistant + EvResult are carried by the returned Reply
		}
	}
}

func oneLine(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func usage() {
	fmt.Fprintln(os.Stderr, `loom — a harness around the harnesses (drive coding-agent CLIs as workers)

usage:
  loom run   --agent claude [--model M] [--dir D] "prompt"     one-shot
  loom chat  --agent claude [--model M] [--dir D]              multi-turn (one msg per stdin line)
  loom panel  --agents claude,agy [--dir D] "prompt"           fan one prompt across agents (council)
  loom review --agents claude,local [--dir R] [--diff HEAD] [files...]   review a diff or files
  loom agents                                                  list backends`)
}
