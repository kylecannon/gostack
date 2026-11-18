# gostack codebase map

*Last Updated: 2026-11-18*

gostack is a Go drop-in replacement for LocalStack Community (and for gresau/localstack-persist). One Go module, `github.com/kylecannon/gostack`, Go 1.27 (toolchain pinned in `go.mod`, run Go through `make`). It serves STS and SQS (Phase 1) and S3 (Phase 2) on :4566 over HTTP/1.1, TLS and h2c. Rules and contracts live in `AGENTS.md`, `docs/architecture.md` and `docs/standards.md`; this map only says where things are.

Flow in one line: `cmd/gostack` builds config, services and the persist engine, hands them to `internal/gateway`, which detects the service (`internal/route`), calls the generated `internal/api/<svc>` dispatcher, which calls the hand-written provider in `internal/service/<svc>`. A service that calls another (S3 notifications to SQS) goes back through the gateway's handler via `internal/inproc`.

| Doc | Read it for |
| --- | --- |
| [architecture.md](architecture.md) | request flow, startup order, persistence and codegen wiring |
| [directory-structure.md](directory-structure.md) | annotated tree |
| [entry-points.md](entry-points.md) | main, HTTP routes, make targets, CI workflows |
| [modules.md](modules.md) | every package: purpose and key files |
| [communication.md](communication.md) | wire protocols, internal API paths, env/config surface |
| [tech-landscape.md](tech-landscape.md) | toolchain, dependencies, Docker, parity and bench tooling |
| [onboarding.md](onboarding.md) | build, test, common tasks |

## How to use / maintain this map

- Read `INDEX.md` (resident), then open the doc for the area you touch.
- Do not edit `internal/api/` by hand; regenerate with `make gen`.
- After structural changes (new package, moved entry point, new service) run `update-codebase-map`. Do not hand-edit `.map-state.json`.
- Style and patterns are not mapped. See `docs/standards.md` and the conventions files named in `AGENTS.md`.
