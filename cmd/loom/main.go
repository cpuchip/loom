// Command loom drives coding-agent CLIs (Claude Code, agy/Gemini) as workers.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

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
	_ = fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return fmt.Errorf("run: need a prompt")
	}
	b, err := pickBackend(*agent)
	if err != nil {
		return err
	}
	sess, err := b.Open(context.Background(), loom.SessionOpts{Workdir: *dir, Model: *model})
	if err != nil {
		return err
	}
	defer sess.Close()
	r, err := sess.Send(context.Background(), prompt)
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
	_ = fs.Parse(args)
	b, err := pickBackend(*agent)
	if err != nil {
		return err
	}
	sess, err := b.Open(context.Background(), loom.SessionOpts{Workdir: *dir, Model: *model})
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
		r, err := sess.Send(context.Background(), line)
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
	replies := loom.Panel(context.Background(), backends, loom.SessionOpts{Workdir: *dir, Model: *model}, prompt)
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

func usage() {
	fmt.Fprintln(os.Stderr, `loom — a harness around the harnesses (drive coding-agent CLIs as workers)

usage:
  loom run   --agent claude [--model M] [--dir D] "prompt"     one-shot
  loom chat  --agent claude [--model M] [--dir D]              multi-turn (one msg per stdin line)
  loom panel --agents claude,agy [--dir D] "prompt"            fan one prompt across agents (council)
  loom agents                                                  list backends`)
}
