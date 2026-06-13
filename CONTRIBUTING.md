# Contributing

## Development Setup

1. Install Go 1.22 or later
2. Clone the repository
3. Run `make build` to compile
4. Run `make test` to run tests

## Making Changes

1. Create a feature branch from `main`
2. Make your changes
3. Add tests for new functionality
4. Run `go vet ./...` and `go test -race ./...` to verify
5. Open a pull request

## Testing

All changes must pass:

- `go vet ./...` — no warnings
- `go test -v -race ./...` — all tests pass
- `golangci-lint run ./...` — no lint errors
- `go build` — clean compilation
- `go mod tidy` — no diff in go.mod/go.sum

Run `make ci` to check everything at once.

## Code Style

- Single binary, single `main.go` file with `// --- Section ---` separators
- No external dependencies beyond the Go standard library and `prometheus/client_golang`
- Follow existing patterns in the codebase
- Keep the code simple and readable

## Reporting Issues

Open an issue on GitHub with:
- What you expected to happen
- What actually happened
- Steps to reproduce
- Go version and operating system