package main

// The mount plan for a commissioned seat — the grounding ruling in code.
//
// Every commissioned seat mounts the FULL WORKSPACE READ-ONLY at /work (memory,
// covenant, intent, journals — everything that makes the workspace rich and
// alive, exactly like Spin's /work). A WRITABLE seat additionally gets two
// writable islands: /commission (a build dir) and /scratch (a memory/journal
// dir). Its claude home is a per-session copy of the commissioned-claude-home
// template, so its CLAUDE.md carries the grounding ritual and its session state
// never collides with a sibling seat's.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cpuchip/loom"
)

// mountInfo records the host paths a plan produced (for the tool result + cleanup).
type mountInfo struct {
	baseHost      string // commissions/<handle>
	homeHost      string // the per-session claude home (mounted ~/.claude, rw)
	workHost      string // the build island (mounted /commission, rw)
	scratchHost   string // the journal island (mounted /scratch, rw)
	workspaceHost string // the workspace root (mounted /work, ro)
}

// dirPlanner builds per-session dirs + SessionOpts under a commissions root.
type dirPlanner struct {
	workspace      string // host path mounted /work READ-ONLY (the whole workspace)
	commissionsDir string // host base for per-session dirs (~/.stewards/commissions)
	homeTemplate   string // ~/.stewards/commissioned-claude-home (seeds each session's home)
	mcpConfigIn    string // in-container --mcp-config path (the substrate hinge), or "" for none
}

// plan creates the session's dirs, seeds its home from the template, and returns
// the SessionOpts that mount everything.
func (p *dirPlanner) plan(handle string, req openReq) (loom.SessionOpts, mountInfo, error) {
	base := filepath.Join(p.commissionsDir, handle)
	home := filepath.Join(base, "home")
	scratch := filepath.Join(base, "scratch")
	work := strings.TrimSpace(req.workdir)
	usingDefaultWork := work == ""
	if usingDefaultWork {
		work = filepath.Join(base, "work")
	}

	for _, d := range []string{home, scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return loom.SessionOpts{}, mountInfo{}, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	if usingDefaultWork {
		if err := os.MkdirAll(work, 0o755); err != nil {
			return loom.SessionOpts{}, mountInfo{}, fmt.Errorf("mkdir %s: %w", work, err)
		}
	} else if fi, err := os.Stat(work); err != nil || !fi.IsDir() {
		return loom.SessionOpts{}, mountInfo{}, fmt.Errorf("designated workdir %q is not a directory", work)
	}
	if err := seedHome(p.homeTemplate, home); err != nil {
		return loom.SessionOpts{}, mountInfo{}, fmt.Errorf("seed claude home: %w", err)
	}

	opts := loom.SessionOpts{
		Isolate:         true, // docker-walled: the seat sees only its mounts, never the host
		SkipPermissions: true, // safe INSIDE isolate (the wall is the container, not a prompt)
		Workdir:         p.workspace,
		WorkdirRO:       true, // /work is grounding, read-only — never an exfil/mutation channel
		ClaudeHome:      home,
		MCPConfig:       p.mcpConfigIn,
	}
	if req.model != "" {
		opts.Model = req.model
	}
	info := mountInfo{baseHost: base, homeHost: home, workHost: work, scratchHost: scratch, workspaceHost: p.workspace}

	if req.writable {
		// Two writable islands beside the read-only workspace.
		opts.ExtraMounts = []string{
			mountVal(work, "/commission", false),
			mountVal(scratch, "/scratch", false),
		}
	} else {
		// Advisory: read-and-answer. No writable islands; the seat's only writes are
		// its own home + the substrate hinge. Consult frames it answer-don't-act.
		opts.Consult = true
	}
	return opts, info, nil
}

// cleanup removes a session's dir tree.
func (p *dirPlanner) cleanup(handle string) error {
	return os.RemoveAll(filepath.Join(p.commissionsDir, handle))
}

// mountVal renders a docker -v value with the host path in Docker-Desktop form.
func mountVal(host, container string, ro bool) string {
	v := filepath.ToSlash(host) + ":" + container
	if ro {
		v += ":ro"
	}
	return v
}

// seedHome copies the authored files of the commissioned-claude-home template
// (CLAUDE.md, settings.json, stewards-mcp.json) into a fresh per-session home and
// ensures a .credentials.json placeholder exists (real creds are layered read-only
// over it by the docker mount). Claude Code fills in the rest at runtime.
func seedHome(template, dst string) error {
	if fi, err := os.Stat(template); err != nil || !fi.IsDir() {
		return fmt.Errorf("template %q missing (run the commissioned-claude-home setup)", template)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"CLAUDE.md", "settings.json", "stewards-mcp.json"} {
		if err := copyFile(filepath.Join(template, name), filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	cred := filepath.Join(dst, ".credentials.json")
	if _, err := os.Stat(cred); os.IsNotExist(err) {
		if err := os.WriteFile(cred, []byte("{}\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
