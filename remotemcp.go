package loom

// remotemcp.go — carry the LOCAL MCP config to a --remote session, so the local
// file stays the single source of truth (closing the "--mcp-config dies at the
// water's edge" gap). The mechanism is one shape for every backend: the config
// bytes ride INSIDE the remote script as base64, are decoded into a private
// /tmp file before the CLI starts, and a shell EXIT trap removes the file when
// the script (and thus the CLI) ends. base64 -d is used because argv-joining
// through `bash -lc` is exactly what mangled inline TOML/JSON values before —
// base64 has no shell metacharacters at all.
//
//	printf '%s' '<b64>' | base64 -d > /tmp/loom-mcp-<rand>.json &&
//	trap 'rm -f /tmp/loom-mcp-<rand>.json' EXIT && <cd …> && <cli …>
//
// The remote is a POSIX box by loom's existing contract (every remote transport
// already runs `bash -lc`); /tmp avoids $HOME expansion issues inside
// shell-quoted argv. The random suffix is minted locally (crypto/rand), so
// concurrent sessions on one target never collide.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
)

// remoteMaterialize builds the prelude that plants data on the remote box.
// Returns the script fragment (ending in " && ") and the remote path the CLI
// should be pointed at.
func remoteMaterialize(data []byte) (prelude, remotePath string) {
	var b [6]byte
	_, _ = rand.Read(b[:])
	remotePath = "/tmp/loom-mcp-" + hex.EncodeToString(b[:]) + ".json"
	b64 := base64.StdEncoding.EncodeToString(data)
	prelude = "printf '%s' '" + b64 + "' | base64 -d > " + remotePath +
		" && trap 'rm -f " + remotePath + "' EXIT && "
	return prelude, remotePath
}

// remoteMCPFromFile materializes a LOCAL config file for a remote session. ok
// is false when path is empty OR the local file does not exist — the latter
// preserves the legacy contract that a --mcp-config naming no local file is a
// REMOTE path, resolved on the target as before.
func remoteMCPFromFile(path string) (prelude, remotePath string, ok bool) {
	if path == "" {
		return "", "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	prelude, remotePath = remoteMaterialize(data)
	return prelude, remotePath, true
}

// replaceFlagValue swaps the value that FOLLOWS the given flag in an argv slice
// (returning a copy); argv without the flag comes back unchanged.
func replaceFlagValue(args []string, flag, newValue string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out)-1; i++ {
		if out[i] == flag {
			out[i+1] = newValue
		}
	}
	return out
}
