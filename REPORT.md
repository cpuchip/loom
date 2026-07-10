# Clone flag report

## Changes

- `cmd/loom/main.go`: adds the documented `-clone` flag to `loom run` and
  `loom chat`, resolves it before a backend is selected or opened, and rejects
  `-clone` with `-remote`. The resolver creates and reports `loom-clone-*`
  temporary workdirs, permits missing or empty explicit destinations, refuses
  non-empty destinations, and returns Git's clone output on failure.
- `cmd/loom/main_test.go`: hermetic local-Git tests create a temporary origin
  repository and cover temporary and empty-destination clones, non-empty
  destination rejection, clone-failure stderr, and remote rejection.
- `README.md`: adds a `loom run --clone` usage example.
- `agy.go`, `backend.go`, `local.go`, `loom_test.go`, and `roleenv_test.go`:
  gofmt-only updates required to make the repository-wide formatting oracle
  clean.

## Oracle outputs

`gofmt -l .`

```
```

`go vet ./...`

```
```

`go test ./...`

```
ok  	github.com/cpuchip/loom	(cached)
ok  	github.com/cpuchip/loom/cmd/loom	(cached)
```

## Resolved ambiguity

`-clone` is intentionally available only on `run` and `chat`, rather than on
other commands that share some session flags. A no-`-dir` clone reports its
temporary directory only after Git succeeds; a failed temporary clone is
removed because it cannot contain a usable session workdir.
