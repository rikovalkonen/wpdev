# wpdev — WordPress local dev CLI (Go)

Minimal MVP of a Lando-like tool targeted for PHP/WordPress development.

## Features (MVP)
- `wpdev init` — create `.wpdev.yml` + templates
- `wpdev start` — render `docker-compose.yml` and start stack
- `wpdev stop` — stop stack
- `wpdev rebuild` — recreate containers
- `wpdev xdebug on|off` — toggle Xdebug mode for PHP-FPM
- `wpdev db:dump` — dump database to `.wpdev/db/dump.sql`
- `wpdev db:import <path>` — import a SQL dump into the DB

## Build project
```bash
# 1) Install Go 1.22+ and Docker Desktop (or Docker Engine)
# 2) Build the CLI
cd wpdev
go mod tidy
go build -o wpdev
```

## Install
```bash
# 1) Make executable
chmod +x ./wpdev

# 2) Copy to bin
sudo mv ./wpdev /usr/local/bin

# 3) If Gatekeeper complains (macOS only)
xattr -d com.apple.quarantine /usr/local/bin/wpdev
```
## Howto use
```bash
# 0) Prerequisites (macOS)
# - Docker Desktop
# - mkcert (only if TLS is enabled): brew install mkcert && mkcert -install
# - wpdev binary (no Go needed if you use the prebuilt binary)

# Verify that tool is working
wpdev --help

# 1) Create a new project (recommended one-shot)
mkdir demo && cd demo
# Interactive setup
wpdev init

# 2) TLS (only if .wpdev.yml → tls.enabled: true)
wpdev tls init

# 3) Start the stack
wpdev start

# Open the site
open https://demo.test   # or http://demo.test if tls.enabled: false

# Common tasks
wpdev db dump                   # writes .wpdev/db/dump-YYYYMMDD-HHMMSS.sql
wpdev db import ./dump.sql
wpdev stop
wpdev rebuild                   # re-render templates & rebuild images

# Switch web server or TLS later
# Edit .wpdev.yml:
#   web.server: apache | nginx
#   tls.enabled: true | false
wpdev rebuild && wpdev start

# Troubleshooting quickies
docker compose ps
docker compose logs --tail=100 caddy php web
docker compose exec caddy caddy validate --config /etc/caddy/Caddyfile

```

## HTTPS with Caddy + mkcert

This extension adds a Caddy reverse proxy that serves HTTPS for:
- `https://<domain>` → WordPress (Nginx)
- `https://mail.<domain>` → Mailpit
- `https://db.<domain>` → Adminer

## Add to wp-config for ssl support
```bash
if (isset($_SERVER['HTTP_X_FORWARDED_PROTO']) && $_SERVER['HTTP_X_FORWARDED_PROTO'] === 'https') {
    $_SERVER['HTTPS'] = 'on';
    $_SERVER['SERVER_PORT'] = 443;
}
```