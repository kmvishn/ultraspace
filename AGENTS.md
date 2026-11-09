# ultraspace Agent Guidelines

## Architecture & Core Constraints
- **Zero-CGO Pure Go**: `ultraspace` builds as a self-contained, statically linked binary (`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`). Never introduce dependencies that require CGO, C libraries, or non-Go toolchains.
- **Vanilla ES Modules (Zero-Build Frontend)**: The frontend in `web/` has NO Node.js, npm, webpack, or external build step.
  - Use vanilla ES modules and the `h(tag, attrs, ...children)` DOM helper from `../core.js`.
  - Register new window apps using `registerApp(...)` from `../wm.js`.
  - Add icons to `web/static/js/icons.js` using 24×24 Lucide-style SVG path strings.
- **Concurrency & Concurrency Safety**: The Go backend manages network state, DNS sinkholes, and system monitoring concurrently. Always guard shared state with `sync.RWMutex` or `sync/atomic`.

## Token & Productivity Discipline
- **Scoped Package Testing**: Never run `go test ./...` across the entire workspace during development. Run tests only for the package being changed (e.g., `go test -race ./internal/sinkhole/...`).
- **Targeted Line Edits**: Never rewrite entire files (like `settings.js` or `sinkhole.go`). Propose targeted search-and-replace edits.
- **Local Dev Server**: Test local backend changes using `go run . -config /dev/null -data ./dev-data -debug` at `http://127.0.0.1:4600`.
