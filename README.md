# ByteProxy Docs

This directory contains additional documentation for the ByteProxy project.

## Contents

- [Getting Started](#getting-started)
- [Configuration Options](#configuration-options)
- [API Reference](#api-reference)
- [Service Management](#service-management)
- [Troubleshooting](#troubleshooting)

## Getting Started

ByteProxy is a powerful API proxy server that forwards requests to various services like Discord, GitHub, and others.

### Prerequisites

- [Bun](https://bun.sh/) runtime (v1.0.0 or later)
- Discord Bot Token (optional)
- GitHub API Token (optional)

### Installation

1. Clone the repository:
   ```bash
   git clone https://github.com/ByteBrushStudios/ByteProxy.git
   cd ByteProxy
   ```

2. Install dependencies:
   ```bash
   bun install
   ```

3. Set up environment variables:
   ```bash
   cp .env.example .env.local
   ```

4. Edit `.env.local` with your API tokens and configuration settings.

5. Start the development server:
   ```bash
   bun run dev
   ```

## Configuration Options

ByteProxy is highly configurable through environment variables and the service registry in `src/config/index.ts`.

### Core Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| PORT | Server port | 3420 |
| CORS_ENABLED | Enable CORS | true |
| CORS_ORIGINS | Allowed origins | * |
| LOG_LEVEL | Logging level (debug, info, warn, error) | info |
| NETWORK_TIMEOUT | Request timeout in ms | 30000 |
| NETWORK_RETRIES | Number of retry attempts | 2 |
| STRICT_TLS | Enforce strict TLS validation | true |

### Security Variables

| Variable | Description | Default |
|----------|-------------|---------|
| REQUIRE_AUTH_FOR_PROXY | Require API key for proxy endpoints | false |
| REQUIRE_AUTH_FOR_MANAGEMENT | Require API key for management endpoints | false |
| PROXY_API_KEY | API key for proxy routes | undefined |
| MANAGEMENT_API_KEY | API key for management routes | undefined |

### Service API Tokens

| Variable | Description | Service |
|----------|-------------|---------|
| DISCORD_BOT_TOKEN | Discord bot token | Discord API |
| GITHUB_TOKEN | GitHub personal access token | GitHub API |

## API Reference

ByteProxy provides a comprehensive API divided into several categories:

### Base Endpoints

- `GET /` - Welcome page with links to documentation
- `GET /up` - Server health and pressure status

### Health Endpoints

- `GET /health` - Basic health check
- `GET /status` - Detailed server status

### Proxy Endpoints

- `ALL /proxy/:service/*` - Forward requests to configured services
  - Example: `GET /proxy/github/users/octocat`

### Management Endpoints

- `GET /manage/services` - List all configured services
- `GET /manage/services/:key` - Get service configuration
- `POST /manage/services` - Add a new service
- `POST /manage/services/:key/test` - Test service connectivity
- `GET /manage/diagnostics` - System diagnostics
- `GET /manage/key-debug` - API key information

### Version Endpoints

- `GET /version/check` - Check for ByteProxy updates

## Service Management

You can dynamically add, configure, and test services through the management API.

### Adding a New Service

```bash
curl -X POST http://localhost:3420/manage/services \
  -H "Content-Type: application/json" \
  -d '{
    "key": "twitter",
    "config": {
      "name": "Twitter API",
      "baseUrl": "https://api.twitter.com/2/",
      "headers": {
        "User-Agent": "ByteProxy/0.1.0"
      },
      "auth": {
        "type": "bearer",
        "tokenEnvVar": "TWITTER_TOKEN"
      }
    }
  }'
```

### Testing Service Connectivity

```bash
curl -X POST http://localhost:3420/manage/services/github/test
```

## Troubleshooting

If you encounter issues, check the following resources:

1. System diagnostics at `/manage/diagnostics`
2. Authentication settings at `/manage/key-debug`
3. Console logs for detailed error messages
4. Environment variable configuration

Common issues:

- **Authentication failures**: Check API tokens in .env.local
- **TLS/SSL errors**: Consider setting STRICT_TLS=false for development
- **404 errors**: Verify service keys and endpoint paths
- **Rate limiting**: Check service configuration for rate limit settings

For additional help, join our [Discord community](https://discord.gg/Vv2bdC44Ge) or create an issue on [GitHub](https://github.com/ByteBrushStudios/ByteProxy/issues).
