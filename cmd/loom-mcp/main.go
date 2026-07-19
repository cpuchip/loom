// Command loom-mcp is a small HTTP MCP server that lets one loom session
// commission and converse with OTHER loom sessions.
//
// It is a ws CLIENT to a local `loom serve` (reusing loom's own ConnectBackend),
// wrapped as an MCP server so the companion seat — the only home wired to it —
// can, through tool calls, open a grounded Claude seat, talk to it, and e-stop it.
// A WRITABLE seat is gated behind a tap-to-approve card in the pg-ai-stewards
// substrate; a read-only advisory seat opens without a tap. See the package files
// for the state machine (manager.go), the mount/grounding plan (mounts.go), and
// the tap gate (gate.go).
//
// Transport mirrors stewards-mcp: the go-sdk StreamableHTTPHandler on /mcp behind
// a constant-time bearer check, bound to loopback (reached from a container via
// host.docker.internal). All logging goes to stderr.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0"

func main() {
	logger := log.New(os.Stderr, "loom-mcp: ", log.LstdFlags)

	home, _ := os.UserHomeDir()
	def := func(rel string) string { return filepath.Join(home, ".loom-mcp", rel) }
	cwd, _ := os.Getwd()

	listen := flag.String("listen", "127.0.0.1:7792", "comma-separated bind address(es). Loopback by default (a container reaches it via host.docker.internal). To expose it to mesh peers, ADD your mesh IP — e.g. --listen 100.x.y.z:7792,127.0.0.1:7792 — so peers reach it while the loopback path (local container, emulator via 10.0.2.2) still works. This is a kill+transcript surface, so the bearer is REQUIRED on every bind (unlike the keyless chat shim); a mesh bind is wall #1 for network peers, the bearer is wall #2. Never bind 0.0.0.0.")
	loomURL := flag.String("loom-url", "ws://127.0.0.1:7791", "the loom serve to commission sessions on")
	loomTokenFile := flag.String("loom-token-file", def("loom-serve-tokens"), "token file for the loom serve (first token used)")
	tokenFile := flag.String("token-file", def("token"), "bearer token gating THIS MCP server (minted if absent)")
	dsn := flag.String("dsn", "postgres://stewards:stewards@localhost:55434/stewards?sslmode=disable", "substrate Postgres DSN (tap gate only)")
	workspace := flag.String("workspace", cwd, "workspace root mounted /work READ-ONLY in every seat")
	commissionsDir := flag.String("commissions-dir", def("commissions"), "host base dir for per-session workspaces")
	homeTemplate := flag.String("home-template", def("commissioned-claude-home"), "the commissioned-claude-home template each seat's home is seeded from")
	mcpConfigIn := flag.String("mcp-config", "/home/node/.claude/stewards-mcp.json", "in-container --mcp-config the seats get (the substrate hinge); empty = none")
	maxSessions := flag.Int("max-sessions", envInt("LOOM_MCP_MAX_SESSIONS", 5), "max concurrent commissioned sessions (env LOOM_MCP_MAX_SESSIONS)")
	sendTimeout := flag.Duration("send-timeout", 10*time.Minute, "wall-clock cap for one session_send turn")
	flag.Parse()

	loomToken, err := firstToken(*loomTokenFile)
	if err != nil {
		logger.Fatalf("read loom serve token: %v", err)
	}
	bearer, err := loadOrMintToken(*tokenFile)
	if err != nil {
		logger.Fatalf("bearer token: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pg, err := newPGGate(ctx, *dsn)
	if err != nil {
		logger.Fatalf("substrate gate: %v", err)
	}
	defer pg.close()
	var gt gate = pg

	pl := &dirPlanner{
		workspace:      *workspace,
		commissionsDir: *commissionsDir,
		homeTemplate:   *homeTemplate,
		mcpConfigIn:    strings.TrimSpace(*mcpConfigIn),
	}
	op := connectOpener{url: *loomURL, token: loomToken}
	mgr := newManager(op, gt, pl, *maxSessions, *sendTimeout, logger.Printf)
	defer mgr.shutdown()

	if err := runHTTP(ctx, mgr, splitAddrs(*listen), bearer, logger); err != nil {
		logger.Fatalf("serve: %v", err)
	}
}

// splitAddrs parses a comma-separated bind list, trimming spaces and dropping
// blanks (so a trailing comma or a spaced list is tolerated).
func splitAddrs(s string) []string {
	var out []string
	for _, a := range strings.Split(s, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// runHTTP serves the MCP tools over the go-sdk StreamableHTTPHandler behind a
// bearer check, on EVERY bind address (mesh IP + loopback). Each MCP session
// gets a fresh *mcp.Server, but the tools close over the ONE shared manager so
// the commission registry is process-wide. Binding is resilient: a single
// address that fails to bind (e.g. the mesh interface momentarily down) is
// logged and skipped so the others still serve; only an all-failed bind is
// fatal. The bearer is required on every bind — the mesh is wall #1 for network
// peers, the token is wall #2 everywhere.
func runHTTP(ctx context.Context, mgr *manager, addrs []string, bearer string, logger *log.Logger) error {
	getServer := func(*http.Request) *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: "loom-mcp", Version: version}, nil)
		registerTools(s, mgr)
		return s
	}
	handler := mcp.NewStreamableHTTPHandler(getServer, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/mcp", bearerAuth(bearer, handler))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	var lns []net.Listener
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			logger.Printf("WARNING: could not bind %s: %v (continuing on the other address(es))", a, err)
			continue
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		return fmt.Errorf("no listen address could be bound: %v", addrs)
	}

	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	}()

	errc := make(chan error, len(lns))
	for _, ln := range lns {
		logger.Printf("loom-mcp %s on http://%s/mcp (commissioning loom serve, gate via pg, auth=%v)", version, ln.Addr(), bearer != "")
		go func(ln net.Listener) {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				errc <- err
			}
		}(ln)
	}

	select {
	case <-ctx.Done():
		logger.Printf("loom-mcp stopped cleanly")
		return nil
	case err := <-errc:
		return err
	}
}

// bearerAuth gates the handler on a constant-time bearer check. On success it
// normalizes the Host to loopback so the go-sdk's DNS-rebinding protection lets a
// host.docker.internal caller through (the same seam stewards-mcp uses).
func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			r.Host = "127.0.0.1"
		}
		next.ServeHTTP(w, r)
	})
}

// firstToken returns the first non-blank, non-comment line of a token file.
func firstToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line, nil
		}
	}
	return "", errEmptyTokenFile{path}
}

type errEmptyTokenFile struct{ path string }

func (e errEmptyTokenFile) Error() string { return "no token in " + e.path }

// loadOrMintToken reads the bearer token, minting + persisting one (0600) if absent.
func loadOrMintToken(path string) (string, error) {
	if tok, err := firstToken(path); err == nil {
		return tok, nil
	} else if !os.IsNotExist(err) {
		if _, ok := err.(errEmptyTokenFile); !ok {
			return "", err
		}
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
