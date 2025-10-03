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

## Quick start
```bash
# 1) Install Go 1.22+ and Docker Desktop (or Docker Engine)
# 2) Build the CLI
cd wpdev
go mod tidy
go build -o wpdev ./cmd/wpdev

# 3) Initialize a project (run in your WP project folder)
./wpdev init

# 4) Start the stack
./wpdev start

# Visit: http://localhost:8080 (WordPress)
# Mailpit: http://localhost:8025
# Adminer: http://localhost:8081

# 5) Toggle Xdebug
./wpdev xdebug on
./wpdev xdebug off

# 6) Stop
./wpdev stop
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
## Notes
- This MVP exposes services on localhost ports for simplicity (no Traefik/mkcert yet).
- File sync uses Docker bind mounts; for macOS/Windows performance, consider Mutagen integration in a later iteration.
- PHP images include Xdebug pre-installed but disabled by default.


---

## HTTPS with Caddy + mkcert

This extension adds a Caddy reverse proxy that serves HTTPS for:
- `https://<domain>` → WordPress (Nginx)
- `https://mail.<domain>` → Mailpit
- `https://db.<domain>` → Adminer

### One-time setup
```bash
# Install mkcert on your host:
# macOS: brew install mkcert nss
# Linux: see https://github.com/FiloSottile/mkcert
# Windows: choco install mkcert

# In your project, generate local certs:
./wpdev tls init      # creates .wpdev/certs for <domain> and wildcard
```

### Start with HTTPS
```bash
./wpdev start
# Visit: https://<domain> , https://mail.<domain> , https://db.<domain>
```

If you skip `tls init`, Caddy will not find the cert files and may fail to start.
