# Changelog

## 0.5 — 2026-10-21

- `pull production` and `pull staging` now fetch WordPress core, themes,
  plugins, MU plugins and site files before localizing the database. New
  projects can pull without preinstalling WordPress or preparing wp-config.php.
- `--db-only` retains the previous database-only behavior. Uploads remain
  optional with `--uploads`; `--uploads-only` still only copies media.
- File transfers are staged and previewed, with joint database/file backups
  and rollback, protection against edits during import, and `pull.exclude`
  patterns for project-specific omissions. Local-only files are retained.

### Upgrading

`pull` now replaces changed WordPress files as well as the database. Use
`--db-only` to keep local code and import only the database. File transfer
requires rsync on both machines. Existing local wp-config.php is retained
as the basis for local settings; uploads remain opt-in.

The release binary targets macOS Apple Silicon (`darwin/arm64`). Other platforms
can build from source.

## 0.4 — 2026-10-21

### Added

- Global local-development plugin lists with automatic installation after
  localization and on start/rebuild, network activation and explicit updates.
- Ignored plugin storage with PHP/Nginx mounts, preservation of project-owned
  copies, and separate database/file rollback for plugin operations.
- Dude remote-media integration for Apache, Nginx and WP-CLI, including
  per-site source uploads maps, multisite switching and local source overrides.

### Upgrading

Plugin automation is opt-in through the user's global plugin list. Add entries
with `wpdev plugins add`, then run `wpdev plugins apply` in an existing project.
Successful localized imports and subsequent start/rebuild commands apply the
list automatically; installed versions only change with `wpdev plugins update`.

Review custom Compose and MU-helper templates with `wpdev templates update`
and merge the generated `.new` versions after `wpdev templates update --apply`.
The new helper and plugin mounts are required for local plugin automation.
For Dude's integration, remove legacy local `SetEnv WP_ENV`,
`SetEnv REMOTE_MEDIA_URL` and `SetEnv WP_HOME` directives when wpdev reports them.

The release binary targets macOS Apple Silicon (`darwin/arm64`). Other platforms
can build from source.

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
