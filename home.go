package loom

import "path/filepath"

// Home returns the resolved loom home directory — the LOOM_HOME override, else ~/.loom,
// else ".loom" when there is no HOME. It is the SAME resolution identity/pins/tokens use
// (see loomDir), so per-run lifecycle records live beside the rest of loom's state.
func Home() string { return loomDir() }

// RunsDir returns the per-run lifecycle-record root: <home>/runs. Each `loom run` writes
// its manifest.json (crash-legible lifecycle), output.log (streamed worker output), and
// a done sentinel under a <run-id> subdirectory here.
func RunsDir() string { return filepath.Join(loomDir(), "runs") }
