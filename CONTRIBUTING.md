# Contributing to aigoproxy

Thanks for your interest. aigoproxy is small on purpose — keep PRs that
way.

## Ground rules

- **Stdlib first.** If a feature can be done with `net/http`, do it with
  `net/http`. External deps require justification.
- **One binary, three surfaces.** Don't split aigoproxy into multiple
  services. If you need a separate process, it should be a separate
  project.
- **Italian for user-facing strings, English for code comments.**
- **Match the style.** `gofmt`, short functions, no naked `interface{}`.

## Development setup

```bash
git clone https://github.com/biodoia/aigoproxy
cd aigoproxy
go test ./...
go run ./cmd/aigoproxy -addr :8080
```

No `go mod tidy` will work offline — we use the local Go module cache
where possible. If you need fresh deps, run `go mod tidy` once online.

## Project layout

```
cmd/aigoproxy/        entrypoint
internal/config/      YAML config types
internal/store/       in-memory + persistent state
internal/proxy/       reverse proxy core
internal/webui/       dashboard
internal/tui/         REPL
internal/mcpserver/   MCP JSON-RPC server
internal/acpserver/   ACP WebSocket server
internal/acme/        Let's Encrypt manager (Wave 2)
docs/                 long-form docs
```

## Testing

```bash
go test ./...                  # all
go test -race ./internal/...   # race detector
```

Write a unit test for any non-trivial change. The MCP server has good
end-to-end coverage; extend it when adding tools.

## Workflow

1. Open an issue for non-trivial changes
2. Fork, branch from `main`
3. Conventional commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`)
4. Push, open PR against `main`
5. Wait for review

## License

By contributing, you agree your contributions are licensed under the
MIT License. See [LICENSE](LICENSE).
