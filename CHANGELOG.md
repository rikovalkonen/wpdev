# Changelog

## 0.3 — 2026-10-21

### Added

- Subdirectory and subdomain multisite setup, conversion, status and DNS
  synchronization, with `wpdev wp` and site management commands.
- Native mkcert trust for PHP and WordPress HTTPS requests inside the project,
  automatic certificate validation/renewal and `wpdev doctor` diagnostics.
- `wpdev pull production` and `wpdev pull staging` using named SSH environments,
  with optional uploads copying, backups and media-only retries.
- SQL/gzip database localization, explicit site URL maps and serialization-aware
  URL replacement. Imports validate in an isolated database before publication
  and restore the local database and managed files if publication fails.
- `wpdev config local` for conditional local database settings while preserving
  the original PHP expressions outside `WPDEV_LOCAL=1`.
- Embedded templates with managed updates, custom-template preservation and
  detection of manual changes to generated Compose files.
- `wpdev --version`, Go tests and opt-in Docker integration suites.

### Changed and fixed

- New projects default to PHP 8.5; existing PHP versions are retained.
- Updated PHP images include Imagick 3.8.1, Xdebug 3.5.3, phpredis 6.3.0 and
  WP-CLI 2.12.0, with AVIF support and version-aware OPcache handling.
- Xdebug toggles control its actual runtime mode; Redis provisions both the
  service and PHP extension.
- Every project command resolves paths from `--config`, and config updates
  preserve unknown YAML fields. Initialization handles existing configuration
  and input/write failures, with MySQL 8.4 as its engine-specific default.
- Database operations stream through the engine's native tools and publish
  backups only after successful completion.
- Corrected Apache/Nginx HTTPS detection, redirects and multisite routing.

### Upgrading

Run `wpdev templates update` to preview template changes, then
`wpdev templates update --apply` and `wpdev rebuild`. Customized templates get
adjacent `.new` files for manual merging. Keep personal Compose additions in
`docker-compose.override.yml`. Existing database storage is preserved.

The release binary targets macOS Apple Silicon (`darwin/arm64`). Other platforms
can build from source. Localization currently supports one WordPress network
per database and the standard WordPress directory layout; custom domain-mapping
plugin routing requires manual adjustment.
