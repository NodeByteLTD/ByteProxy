# ByteProxy

A small HTTP proxy that sits between your services and third-party APIs such as Discord and GitHub. It follows each API's rate limits, retries short 429s, and forwards requests and responses unchanged, byte for byte.

ByteProxy is written in Go and builds to a single static binary with no runtime dependencies.

## Quick start

```sh
cp .env.example .env
# set PROXY_API_KEY and MANAGEMENT_API_KEY
make run
```

The proxy listens on port `3420` by default. The API reference is at `http://localhost:3420/docs`, and the raw OpenAPI 3.1 spec is at `/openapi.json`.

```sh
curl http://localhost:3420/proxy/discord/v10/users/@me \
  -H "X-Api-Key: $PROXY_API_KEY" \
  -H "Authorization: Bot $DISCORD_TOKEN"
```

## How requests are proxied

A request to `/proxy/{service}/{path}` is sent to the service's base URL plus `{path}`, with the same method, query string, body and headers. The response status, headers and body are returned as is, and the proxy adds `X-Proxy-Service` and `X-Proxy-Duration`.

Before a request is sent upstream, the proxy removes:

- its own credentials (`X-Api-Key`, the `api_key` query parameter, and an `Authorization: Bearer` header that holds the proxy key)
- cookies
- hop-by-hop headers
- client forwarding headers (`X-Forwarded-*`, `Forwarded`, `X-Real-Ip`, Cloudflare headers)

### Built-in services

| Key | Upstream | Rate limiting |
|---|---|---|
| `discord` | `https://discord.com/api/` | Per credential. Follows bucket hashes and major parameters, with a global requests-per-second cap. |
| `github` | `https://api.github.com/` | Per credential and resource (core, search, graphql). |

For Discord, `/proxy/discord/v10/...` and `/proxy/discord/api/v10/...` both map to `https://discord.com/api/v10/...`. A 429 is retried up to 3 times when the wait is 30 seconds or less. Longer waits are returned to the caller with Discord's original body. A global 429 pauses every request that uses that credential.

## Authentication

The proxy and the upstream API are authenticated separately.

### Proxy auth

When `REQUIRE_AUTH_FOR_PROXY` is on, `/proxy/*` accepts the key in any of these forms:

- `X-Api-Key: <key>`
- `Authorization: Bearer <key>`
- `?api_key=<key>`

`/manage/*` uses `MANAGEMENT_API_KEY` in the same way. If auth is required but its key is empty, the proxy refuses to start.

### Upstream credentials

The proxy does not hold a Discord or GitHub token of its own. Each caller sends its own credential, and the proxy uses the first one it finds:

1. `X-Upstream-Authorization`
2. `Authorization`, unless it is the proxy key sent as a Bearer token
3. For dynamic services only, the env var named in the service's `auth.tokenEnvVar`

Use `X-Upstream-Authorization` when you also use the `Authorization` header to send the proxy key:

```sh
curl http://localhost:3420/proxy/github/user \
  -H "Authorization: Bearer $PROXY_API_KEY" \
  -H "X-Upstream-Authorization: Bearer $GITHUB_TOKEN"
```

Rate limit state is tracked per credential, so several services can share one proxy without sharing limits. Credentials are hashed before they are used as keys, and are never logged.

## Dynamic services

You can add other APIs at runtime through the management API. They are saved to `SERVICES_FILE` and loaded again on startup.

```sh
curl -X POST http://localhost:3420/manage/services \
  -H "X-Api-Key: $MANAGEMENT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "key": "status",
    "config": {
      "name": "Status API",
      "baseUrl": "https://status.example.com/api/",
      "headers": { "Accept": "application/json" },
      "rateLimit": { "maxRequests": 60, "windowMs": 60000 },
      "auth": { "type": "bearer", "tokenEnvVar": "BYTEPROXY_TOKEN_STATUS" }
    }
  }'
```

- `key` must match `^[a-z0-9][a-z0-9_-]{0,31}$`.
- `baseUrl` must be `http` or `https`.
- Static `headers` cannot set `Authorization`, `Cookie`, `Host` or other headers that carry credentials or control framing. Use `auth` for credentials.
- `auth.type` is `bearer`, `bot`, `basic` or `api-key`. `api-key` also needs `headerName`.
- `auth.tokenEnvVar` must be named `BYTEPROXY_TOKEN_*`, so a management key cannot read arbitrary environment variables such as a database URL.

A credential sent by the caller always takes precedence over the configured token. Built-in services cannot be removed.

## Endpoints

| Route | Auth | Purpose |
|---|---|---|
| `GET /` | none | Service info |
| `GET /health`, `/status` | none | Health, uptime and memory |
| `GET /up` | none | Liveness check, served even under memory pressure |
| `GET /auth-status` | none | Which auth modes are enabled |
| `GET /version`, `/version/check` | none | Current version and release check |
| `GET /openapi.json`, `/docs` | none | API spec and reference |
| `/proxy/{service}/{path}` | proxy | Forward a request |
| `GET /proxy/services` | proxy | List services |
| `GET /proxy/services/{service}/rate-limit` | proxy | Window limit status for a service |
| `GET /proxy/auth-debug`, `/auth-test`, `/key-debug` | proxy | Auth troubleshooting |
| `GET`, `POST /manage/services` | management | List or add services |
| `GET`, `DELETE /manage/services/{key}` | management | Inspect or remove a service |
| `POST /manage/services/{key}/test` | management | Check that a service's upstream is reachable |
| `GET /manage/diagnostics`, `/manage/key-debug` | management | Configuration diagnostics |

Errors are returned as JSON with an `error` message, for example `{"error": "Service 'foo' not configured"}`.

| Status | Meaning |
|---|---|
| 401 | Missing or wrong proxy or management key |
| 404 | Unknown service or route |
| 413 | Request body larger than `MAX_BODY_MB` |
| 429 | Rate limited by the proxy or upstream |
| 502 | Upstream unreachable or TLS failure |
| 503 | Memory pressure above `MAX_HEAP_MB` or `MAX_RSS_MB` |
| 504 | Upstream did not respond within `NETWORK_TIMEOUT` |

## Configuration

Settings are read from the environment. `.env.local` and `.env` are also loaded, but they never override variables that are already set.

| Variable | Default | Description |
|---|---|---|
| `PORT` | `3420` | Listen port |
| `APP_ENV` | `local` | `production`, `development` or `local`. `development` skips the update check. Falls back to `NODE_ENV`. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `PROXY_API_KEY` | | Key for `/proxy/*` |
| `MANAGEMENT_API_KEY` | | Key for `/manage/*` |
| `REQUIRE_AUTH_FOR_PROXY` | `false` | Require `PROXY_API_KEY` |
| `REQUIRE_AUTH_FOR_MANAGEMENT` | `false` | Require `MANAGEMENT_API_KEY` |
| `CORS_ENABLED` | `true` | Send CORS headers |
| `CORS_ORIGINS` | `*` | Comma-separated allowed origins. Credentials are only allowed for listed origins, never for `*`. |
| `NETWORK_TIMEOUT` | `30000` | Upstream timeout in milliseconds |
| `STRICT_TLS` | `true` | Verify upstream certificates |
| `MAX_BODY_MB` | `100` | Largest request body accepted |
| `MAX_HEAP_MB` | `512` | Heap size above which requests get 503 |
| `MAX_RSS_MB` | `1024` | Total memory held by the Go runtime above which requests get 503 |
| `DISCORD_GLOBAL_RPS` | `50` | Discord global requests per second, per credential |
| `SERVICES_FILE` | `data/services.json` | Where dynamic services are stored |
| `SKIP_UPDATE_CHECK` | `false` | Skip the release check on startup |
| `UPDATE_REPO` | `NodeByteLTD/ByteProxy` | GitHub repo checked for releases |
| `BYTEPROXY_TOKEN_*` | | Upstream tokens that dynamic services may use |

## Development

| Command | What it does |
|---|---|
| `make run` | Run from source |
| `make build` | Build `bin/byteproxy`, with the version taken from git |
| `make test` | Run the tests |
| `make race` | Run the tests with the race detector |
| `make cover` | Print test coverage per function |
| `make lint` | Check formatting and run `go vet` |
| `make check` | Run lint and the tests |

Requires Go 1.25 or newer. The tests start real HTTP servers on localhost and need no network access.

## Deployment

The repo includes a [Railpack](https://railpack.com) config (`railpack.json`), which builds the binary with the Go provider. It sets `SERVICES_FILE=/data/services.json`, so mount a persistent volume at `/data` or dynamic services are lost on redeploy.

To run it anywhere else, build with `make build` and run `bin/byteproxy`. It handles SIGINT and SIGTERM by finishing in-flight requests before exiting.

## Migrating from 1.x

- Remove `DISCORD_BOT_TOKEN` and `GITHUB_TOKEN`. They are ignored, and a warning is logged if they are set. Every caller must now send its own upstream credential in `Authorization` or `X-Upstream-Authorization`.
- A caller that sends the proxy key as `Authorization: Bearer` must move its upstream credential to `X-Upstream-Authorization`.
- Rename any token env var used by a dynamic service to `BYTEPROXY_TOKEN_*`.
- Dynamic services are now saved to disk. Services added under 1.x were only held in memory and must be added again once.
- If `REQUIRE_AUTH_FOR_PROXY` or `REQUIRE_AUTH_FOR_MANAGEMENT` is `true`, set the matching key. 1.x started without auth in that case, and 2.x refuses to start.
- The Swagger UI has been replaced by `/docs` and `/openapi.json`.

## License

See [LICENSE](LICENSE).
