# AGENTS.md

## Build / Test / Lint
- **Test all**: `go test ./...`
- **Single test**: `go test -run TestFuncName ./...`
- **Lint**: `golangci-lint run -v ./...` (config: `.golangci.yml` v2)
- **Format**: `gofmt -s -w . && goimports -w .` then `gci write --skip-generated --custom-order -s standard -s "prefix(github.com/ysicing/caddy2-geocn)" -s default -s blank -s dot .`
- **Build with xcaddy**: `xcaddy build --with github.com/ysicing/caddy2-geocn=../caddy2-geocn`

## Architecture
Single Go package `geocn` — a Caddy v2 plugin providing two HTTP request matchers:
- **`geocn`** (`geocn.go`): Matches Chinese IPs via MaxMind GeoIP2 mmdb (`Country.mmdb`). Global app `GeoCNApp` manages DB lifecycle, caching, and periodic remote updates. Lightweight `GeoCN` matcher references the app via `ctx.App("geocn")`.
- **`geocity`** (`geocity.go`): Matches by region/province/city via ip2region xdb (`ip2region_v4.xdb`, `ip2region_v6.xdb`). Global app `GeoCityApp` manages DB lifecycle, caching, and periodic remote updates. Lightweight `GeoCity` matcher references the app via `ctx.App("geocity")`.
- **`common.go`**: Shared utilities (HTTP download, file ops, generic TTL `Cache[T]`).
- Tests use fixture files in the repo root; test helpers use `package geocn` (not `_test`).

## Code Style
- Go tabs indentation. Imports ordered: stdlib, project, third-party (enforced by `gci`).
- Caddy module interfaces enforced via compile-time `var _ Interface = (*Type)(nil)` checks.
- Errors: `fmt.Errorf` with `%w` wrapping; structured logging via `go.uber.org/zap`.
- No exported constructors — modules are instantiated by Caddy's module system via `Provision()`.
- JSON struct tags use `snake_case` with `omitempty`. Caddyfile parsing in `UnmarshalCaddyfile`.

## Caddy Extension Best Practices
- **Module lifecycle**: `New()` → JSON unmarshal → `Provision()` → `Validate()` → use → `Cleanup()`. Respect this order.
- **Provision vs Start**: `Provision()` only sets defaults and initializes fields. No network I/O or expensive ops — `caddy validate` also calls `Provision()`. For `caddy.App` modules, put expensive work in `Start()`.
- **Validate()**: Implement `caddy.Validator` to separate config validation from initialization. Must be read-only.
- **Interface guards**: Every module must have compile-time interface checks (`var _ caddy.Provisioner = (*Type)(nil)`) at file top, including `caddy.Validator`.
- **Logger**: Use `ctx.Logger()` (no arguments). The old `ctx.Logger(module)` API is deprecated.
- **Caddyfile docs**: Document syntax in godoc comment above `UnmarshalCaddyfile` methods.
- **Hot reload safety**: New modules start before old ones stop. Use `caddy.UsagePool` for shared global state. Avoid bare global variables.
- **Struct fields with locks**: Use pointer types (`*sync.RWMutex`) for sync primitives in structs that have value-receiver methods (e.g. `CaddyModule()`), to avoid copylocks violations. Don't use `sync.Once` when Caddy guarantees single `Provision()`/`Start()` calls per instance.
- **Default values**: Set all defaults in `Provision()`, not in runtime methods like `periodicUpdate()`.
