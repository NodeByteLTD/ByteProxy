# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## [2.0.0] - Unreleased

A rewrite of ByteProxy in Go. The HTTP routes are kept compatible with 1.x, but credential handling has changed. See the migration notes in the README.

### Added

- The service is a single static binary built only on the Go standard library.
- An OpenAPI 3.1 spec is served at `/openapi.json`, with interactive reference docs at `/docs`. A test fails if any route is missing from the spec.
- `X-Upstream-Authorization` header, so a caller can authenticate to the proxy with `Authorization: Bearer` and still send its own upstream credential.
- Discord rate limiting per credential, following Discord's bucket hashes and major parameters, with a global requests-per-second cap (`DISCORD_GLOBAL_RPS`). A 429 is retried when the wait is short, and returned to the caller when it is longer than 30 seconds.
- GitHub rate limiting per credential and resource (core, search, graphql).
- Services added at runtime are saved to `SERVICES_FILE` and loaded again on startup.
- `/up` liveness endpoint that is always served, even when memory pressure causes other requests to return 503.
- Graceful shutdown on SIGINT and SIGTERM.
- Makefile, Railpack config and `.env.example`.

### Changed

- The proxy no longer uses its own Discord or GitHub token. Callers send their own credential with each request. `DISCORD_BOT_TOKEN` and `GITHUB_TOKEN` are ignored, and a warning is logged if they are set.
- Dynamic services can only read upstream tokens from env vars named `BYTEPROXY_TOKEN_*`.
- If auth is required but no key is configured, startup fails with an error. Previously the proxy started without auth.
- An update check that fails or finds a newer release logs a warning and never stops the process.
- A configured service `User-Agent` always takes precedence over the caller's.

### Fixed

- Request and response bodies are forwarded as raw bytes, so multipart uploads and binary responses (images, archives) arrive intact.
- A compressed upstream response is decoded once. `Content-Encoding` is no longer forwarded alongside an already decoded body.
- Upstream timeouts return 504, and unreachable upstreams return 502, instead of a generic 500.
- Encoded path segments, such as emoji in Discord reaction routes, are forwarded without being decoded.

### Security

- API keys are compared in constant time.
- `api_key` query parameters, `X-Api-Key`, cookies and client forwarding headers are removed before a request goes upstream.
- Key debug and diagnostics endpoints report only whether a key is configured, never any part of it. `/auth-status` no longer echoes the supplied key.
- Upstream CORS headers are replaced with the proxy's own policy. A wildcard origin never allows credentials.
- TLS 1.2 is the minimum version, and certificate verification cannot be skipped unless `STRICT_TLS=false` is set explicitly.
