# wpdev — local WordPress development

A Go CLI for Docker-based WordPress development with Apache or Nginx/PHP-FPM,
mkcert HTTPS, and subdirectory or subdomain multisite networks.

## Requirements and installation

- Go 1.22+ to build the CLI; Docker Engine/Desktop and Compose v2.20+ to run it.
- `mkcert` when TLS is enabled. On macOS: `brew install mkcert`.
- Local DNS for your `.test` domains (see below).

Download the macOS Apple Silicon binary and its SHA-256 checksum from
[release 0.5](https://github.com/rikovalkonen/wpdev/releases/tag/0.5), or build
from source on other platforms:

```sh
go build -o wpdev .
install -m 755 wpdev /usr/local/bin/wpdev
wpdev --version
```

PHP 8.5 is the default for new projects. Existing PHP versions are preserved;
PHP 8.3, 8.4 and 8.5 are covered by the Docker build tests. Images include
Imagick 3.8.1, AVIF-capable GD, OPcache, Xdebug 3.5.3, phpredis 6.3.0 and
WP-CLI 2.12.0. Xdebug's modes are off by default.

## Quick start

```sh
mkdir example && cd example
wpdev init                         # interactive; --defaults is also available
wpdev start
wpdev doctor
```

`init` creates `.wpdev.yml` and scaffolds the document root. It installs the
mkcert CA when TLS is enabled; the OS may request authorization for the trust
store. Re-running init and declining overwrite uses the existing configuration.
`init --defaults` also preserves an existing configuration.

Use existing WordPress files, or prepare a new installation:

```sh
wpdev wp core download --force
wpdev wp config create --dbname=wordpress --dbuser=wp --dbpass=secret --dbhost=db
wpdev start                        # activates the local WordPress HTTPS helper
wpdev wp core install --url=https://example.test --title=Example \
  --admin_user=admin --admin_email=admin@example.test --skip-email
```

The initial placeholder `index.php` is replaced by `core download --force`.
WP-CLI prints a generated admin password if none is supplied. Configure Apache
permalinks with `wpdev wp rewrite structure '/%postname%/' --hard` or save them
in WordPress. wpdev supplies the Apache module information WP-CLI needs to write
`.htaccess`. With Nginx, use the same command without `--hard`; Nginx already
includes the single-site front-controller rules.

All project commands support an explicit configuration file. Paths in the
configuration resolve from its directory, and Compose runs there too:

```sh
wpdev --config /path/to/project/.wpdev.yml start
wpdev --config /path/to/project/.wpdev.yml wp plugin list
```

Put wpdev's `--config` **before** `wp`; all arguments after `wp` are passed
unchanged to WP-CLI. WP-CLI runs with the host UID/GID when available so files
created in the bind mount belong to the developer.

## Configuration

```yaml
name: example
domain: example.test
web:
  server: apache                  # apache | nginx
  php: "8.5"
  docroot: wp                     # relative directory, or .
database:
  engine: mariadb                 # mariadb | mysql
  version: "11.4"                 # init suggests 8.4 for MySQL
  portforward: "3307"             # empty string disables the host port
  persist: bind                  # bind | volume
  data_path: database             # used for bind mounts
services:
  redis: true
  mailpit: true
  adminer: true
xdebug:
  enabled: false
tls:
  enabled: true
multisite:
  enabled: false
  mode: subdirectory              # subdirectory | subdomain
  base_path: /
```

When enabled, Mailpit is served at `mail.example.test` (SMTP port 1025 inside
the network), Adminer at `db.example.test`, and Redis at `redis:6379`.
The Redis toggle provisions Redis and its PHP client; install/configure your
preferred WordPress object-cache plugin separately. Database credentials are
local development defaults: database `wordpress`, user `wp`, password `secret`;
root password `root`. Database service hostname: `db`.

The old `perf` fields remain readable and preserved; synchronization currently
uses bind mounts. Unknown YAML fields and comments are retained when wpdev
updates settings. Existing configs without a persistence setting retain the
named `dbdata` volume. The Compose project name continues to derive from the
project directory (or `COMPOSE_PROJECT_NAME`), not `name` in the YAML.

## Multisite

Choose the network type in `init`, or set `multisite.enabled: true` and
`multisite.mode` in your existing configuration. Both network types use the same
`.wpdev` directory and support Apache and Nginx.

Prepare WordPress files and `wp-config.php` using the commands above, then:

```sh
wpdev start
# Empty WordPress database:
wpdev multisite install --title="Local network" \
  --admin-user=admin --admin-email=admin@example.test
# OR an existing single-site installation:
wpdev multisite convert --title="Local network"

wpdev site create child --title="Child site"
wpdev site list
wpdev multisite status
wpdev multisite sync
```

Missing title/administrator details are prompted for. For automation, supply
all required flags. An optional `--admin-password` is available for install;
omitting it generates a password. Installation skips notification email.

Conversion first backs up the database to `.wpdev/db/` and `wp-config.php` and
`.htaccess` to timestamped `.wpdev/backups/` directories. A failed backup aborts
the conversion. Apache updates only the marked WordPress rewrite block and
retains custom directives outside it. Review backups if a later step fails;
wpdev does not automatically reverse a partially completed database operation.

A subdirectory site is available at `example.test/child/`; a subdomain site at
`child.example.test`. Caddy routes the network, and `sync` reads WordPress's
registered site domains into `.wpdev/sites.json` for Docker DNS aliases. Start
and `site create` synchronize automatically. After adding a subdomain through
the dashboard or raw `wp` passthrough, run `wpdev multisite sync`.

`mail` and `db` subdomains are reserved when their corresponding services are
enabled. Use the localization commands below to bring production domains into
the local network; externally mapped domains require an explicit URL map.
Changing the YAML network type/base/domain alone does not convert an existing network.
A matching existing network is reused rather than reinstalled.

`base_path` is the URL prefix, independent of the physical document root; it
must start and end with `/`. The standard WordPress layout is expected:
`wp-config.php` and `wp-content/` inside `web.docroot`.

## Importing production into a local project

With a configured SSH environment, `pull` downloads WordPress core, themes,
plugins, MU plugins and other site files into `web.docroot`, then localizes the
database. It also works in a newly initialized project with no WordPress files.
The source's core version is retained. Both `production` and `staging` use this
same flow:

```sh
wpdev pull production
wpdev pull production --uploads
wpdev pull staging --dry-run
wpdev pull staging
# Keep existing local code and import only the database:
wpdev pull production --db-only
# Retry media without importing the database again:
wpdev pull production --uploads-only

# SQL imports require WordPress files and wp-config.php already in the project:
wpdev db import production.sql.gz --localize --dry-run
wpdev db import production.sql.gz --localize
# Localize a database already imported into the running db service:
wpdev db localize
# Only add conditional local DB settings to wp-config.php:
wpdev config local
```

Full pulls download files to a temporary directory. The staged WordPress core
validates the imported database before any active files change. Existing local
`wp-config.php` is retained as the basis for localization; in a new project it
is fetched from the source, including the standard parent-directory location.
Newly fetched `wp-config.php` is added to the project's `.gitignore`.
Changed files are listed before the single database/file replacement confirmation
(`--yes` accepts it), and are backed up with the database. Failed publication
restores both. Files edited locally while validation runs abort the pull.
Additional local files are retained; full pulls do not mirror deletions.

Uploads remain optional (`--uploads`) so remote media can be used without copying
the media library. Git metadata, wpdev/Compose/WP-CLI configuration, `.env` files,
root SQL dumps/logs, known cache/backup directories and production cache/database drop-ins
are excluded. Root `.htaccess`, `.user.ini` and `php.ini` stay local; wpdev generates
the WordPress rewrite rules. Managed local-development plugins are preserved.
Remote symlinks and collisions with local symlinks stop the pull with a file path
to review. Custom WordPress content/plugin directory layouts require `--db-only`
and prepared local files.

Additional exclusions use rsync patterns relative to the remote WordPress root:

```yaml
pull:
  exclude:
    - /wp-content/themes/my-local-theme
    - /private-backups
```

`db import` without `--localize` remains a streaming SQL import, with gzip
auto-detection. Paths supplied on the command line are relative to your shell's
current directory. Put the global `--config` before the command when working
with another configuration file.

Configure SSH in `.wpdev.yml`:

```yaml
ssh:
  production:
    host: production             # hostname or ~/.ssh/config alias
    user: deploy                 # optional; otherwise use SSH defaults
    port: 22                     # optional
    wp_path: /srv/www/wordpress   # absolute path containing WordPress
  staging:
    host: staging                # hostname or ~/.ssh/config alias
    user: deploy
    wp_path: /srv/www/wordpress
```

Environment names are keys under `ssh`: `pull staging` uses `ssh.staging`.
Every environment supports the same localization, backup and uploads options.
Replace these example connection settings with your own.

SSH uses the host's normal configuration, known hosts and keys/agent, with
batch authentication. The remote needs WP-CLI and its database dump utility;
file and media copying additionally need rsync on both ends. The remote operations
read installation metadata and export the installation's prefixed tables.
Remote credentials are not written to the local YAML.

Localization builds a reusable PHP helper image and imports into a separate
temporary database container using the project's database engine/version.
Its network is internal, the project mount is read-only, and local plugins,
themes, MU plugins and cache drop-ins are excluded from staged WordPress
bootstrap. The dump is never executed against the active database during
validation. `--dry-run` performs this validation and previews URLs without
publishing database or project-file changes.

The local domain and protocol come from `domain` and `tls.enabled`. Network
type, base path, site IDs and table prefix are retained from the source.
The main domain, subdirectory sites and ordinary subdomains are mapped
automatically. A standalone domain needs a YAML map keyed by its blog ID:

```yaml
sites:
  7: https://masterportaali.test/asiakas/
```

Pass it with `--map local-sites.yml`. Subdirectory networks use paths on the
project domain; subdomain networks may use one-level subdomains. Unknown IDs,
collisions and reserved service names abort the import. Use `--table-prefix`
if several installations are present, and `--multisite-mode subdirectory`
or `subdomain` if the source type cannot be inferred from its config/sites.
These flags are supported by `pull`, `db localize` and `db import --localize`.

Network tables and per-site `home`/`siteurl` values are updated explicitly.
URL replacement covers serialized data and other prefixed tables, including
JSON-escaped URLs. More specific URLs are handled before their parent, and
two passes avoid cascading replacements. GUIDs and email addresses are
retained. A single site is published at the local domain root. Because
`web.docroot` contains WordPress core, a separate production core URL such as
`/wp` is mapped to the local site's root rather than retaining a redundant
directory prefix. Multisite network base paths are retained.

`config local` and localization preserve original PHP value expressions.
Only `WPDEV_LOCAL=1` selects local DB credentials; the original values apply
elsewhere. The managed comment markers make repeated runs idempotent.
Definitions must be unambiguous, direct top-level `define()` statements
(and a direct `$table_prefix` assignment for localization). Included,
conditional or duplicate definitions produce an actionable error. The
transformer uses PHP tokens and syntax validation before replacing the file.
Production global URL/cookie constants are skipped locally in multisite so
each site's database settings can apply.

Before publication, wpdev asks once to replace the local database; `--yes`
confirms this for automation. It backs up the database and managed files,
stops the existing local services, publishes the transformed database, updates
config/routing/TLS, and verifies WordPress and internal HTTP requests. A
failure restores the database, files and previously running services. Recovery
files and `RECOVERY.txt` are under `.wpdev/backups/import-*`; SQL backups are
under `.wpdev/db`. A failed recovery reports those paths explicitly.

`--uploads` copies media after a successful database import, keeps additional
local files and backs up replaced files. An interrupted media transfer reports
that the database succeeded and can be retried with `--uploads-only`.

The first version supports one WordPress network per database and the standard
WordPress directory layout. Domain-mapping plugins with their own routing
require manual adjustment. Tables without primary keys, views, stored routines
and triggers require manual migration. Imports do not convert a subdirectory
network to subdomains. SQL imports and `pull --db-only` use existing local code;
normal SSH pulls copy code from the selected environment.

## Global local-development plugins

Keep a personal plugin list in `$XDG_CONFIG_HOME/wpdev/config.yml`, or
`~/.config/wpdev/config.yml` when `XDG_CONFIG_HOME` is unset. An absent file means
no automatic plugin installs. These commands manage that global list and work
outside a project:

```sh
wpdev plugins add dude-load-media-from-remote \
  --source https://github.com/digitoimistodude/dude-load-media-from-remote/archive/refs/heads/main.zip \
  --integration remote-media
wpdev plugins add query-monitor
wpdev plugins list
wpdev plugins remove query-monitor
```

The equivalent configuration is:

```yaml
plugins:
  dude-load-media-from-remote:
    source: https://github.com/digitoimistodude/dude-load-media-from-remote/archive/refs/heads/main.zip
    integration: remote-media
  query-monitor:
    source: query-monitor
```

`start`, `rebuild` and successful localized imports (`pull`, `db import
--localize`, `db localize`) install missing entries and activate them. Multisite
activation covers the entire network, including future subsites. WordPress
must already be installed; otherwise wpdev defers plugins until the next start.
Existing versions, including GitHub branch ZIPs, are never updated implicitly.

```sh
wpdev plugins apply --dry-run
wpdev plugins apply
wpdev plugins update                  # explicitly update managed entries
wpdev plugins update query-monitor
```

In `.wpdev.yml`, opt out for a whole project or selected entries:

```yaml
local_plugins:
  enabled: true                       # false disables the inherited list
  disabled:
    - query-monitor
remote_media:
  enabled: true                       # false disables the remote-media adapter
  # Optional source-home overrides; normally inferred from the imported DB:
  # url: https://production.example
  # sites:
  #   7: https://customer.example
```

Downloaded plugins live in ignored `.wpdev/plugins/` directories and are mounted
read-only into the usual plugin paths in PHP and Nginx. Their assets therefore
work normally without adding plugin source files to your project checkout.
Use `wpdev plugins update` to update these copies, rather than the WordPress
dashboard. A project-owned plugin with the same ID is reused and can be activated,
but its files, updates and later deactivation remain under your control.
Removing or disabling a global entry deactivates only wpdev-managed copies on the
next apply/start; cached packages remain available.

Plugin changes have their own database/file backups under `.wpdev/backups/plugins-*`
and `.wpdev/db`, and a failed operation restores its starting state. A plugin
failure after import reports that the database import succeeded; rerun
`wpdev plugins apply` after fixing the issue. The global list accepts WordPress.org
slugs and public HTTPS ZIP URLs; private package authentication is not supported.

### Dude remote-media integration

With `integration: remote-media`, `pull staging` uses staging's original media
addresses and `pull production` uses production's. SQL localization also retains
the source URLs. The import records each site's original home, core URL and uploads
base before URL replacement in ignored `.wpdev/remote-media.json`. Re-localizing
an already local database retains its original source map.

The local MU helper supplies `WP_ENV=development`, the current site's local
`WP_HOME` environment value and its `REMOTE_MEDIA_URL`. It handles multisite
context switches and corrects the uploads prefix through Dude's filter, including
separate core directories, mapped domains and `uploads/sites/<id>/` paths.
Existing local media stays local. No global `WP_HOME` PHP constant is added.

The adapter works in Apache, Nginx and WP-CLI, with HTTP or HTTPS, and is guarded
by `WPDEV_LOCAL=1`. Remove old local `.htaccess` `SetEnv WP_ENV`,
`SetEnv REMOTE_MEDIA_URL` and `SetEnv WP_HOME` directives: wpdev reports these
conflicts instead of overwriting user rules. Without imported source metadata or
an explicit `remote_media.url`/`sites` override, image URLs remain local.

## HTTPS and internal requests

`tls.enabled: true` enables the whole local trust chain:

1. Discover the CA with `mkcert -CAROOT`.
2. Generate and validate main-domain and wildcard certificates.
3. Copy only the public `rootCA.pem` into `.wpdev/certs/`.
4. Mount it into PHP and refresh the system CA bundle at container startup.
5. Resolve project hostnames directly to Caddy inside the project's Docker network.

The private CA key is never copied into the project or image. Leaf private keys
are local files with restricted permissions. Certificates are renewed when
invalid, mismatched, or within 30 days of expiry. A changed CA or leaf certificate
causes the affected containers to be recreated on the next start.

PHP cURL, streams and command-line clients use the system bundle, which retains
public CAs. WordPress normally selects its own CA bundle: wpdev therefore
mounts a generated helper and creates an ignored, guarded loader at
`wp-content/mu-plugins/wpdev-local.php`. The loader is inert outside the local
container. The helper changes the CA bundle only for registered local HTTPS
hosts and never disables verification. Existing user code at that loader path
is not overwritten. Custom content-directory layouts require a custom integration.

```sh
wpdev tls init                    # install/verify CA and renew certificates
wpdev start                       # apply changed trust and certificates
wpdev doctor                      # check DNS, TLS and HTTP clients
```

With TLS disabled, mkcert is not required. A single project owns host ports
80/443 by default; cross-project routing is outside the current scope.

### Local DNS on macOS

For wildcard `.test` resolution:

```sh
brew install dnsmasq
printf 'address=/.test/127.0.0.1\n' >> "$(brew --prefix)/etc/dnsmasq.conf"
sudo brew services restart dnsmasq
sudo mkdir -p /etc/resolver
printf 'nameserver 127.0.0.1\n' | sudo tee /etc/resolver/test
```

For a single domain, `/etc/hosts` entries also work; include `mail`/`db` and each
subdomain you use. Host DNS and Docker DNS are separate: a wildcard certificate
does not provide either. `doctor` checks both. On Linux, configure the local
resolver/hosts equivalently. wpdev does not edit system DNS configuration.

## Updating existing projects

```sh
wpdev templates update             # preview differences; no writes
wpdev templates update --apply     # update managed defaults; custom .new files
wpdev rebuild
```

The CLI embeds current templates. Missing templates and recognized unmodified
legacy defaults are updated automatically on start. Customized templates remain
active and produce a notice. `templates update --apply` writes their proposed
replacement next to the original as `*.new`; merge your changes manually.
To explicitly restore all defaults, including authorization to regenerate a
manually changed Compose file:

```sh
wpdev templates update --apply --reset-custom
```

That command backs up replaced templates and `docker-compose.yml` first. Move
personal Compose additions to `docker-compose.override.yml` before resetting.
The override file is included for validation and every Compose command. A
manually edited generated Compose file otherwise blocks regeneration.
Template rendering and Compose validation happen before active generated files
are replaced. Custom templates must be reviewed for new TLS/multisite features;
retaining an old PHP or Compose template can retain its old behavior.

Do not copy project-specific remote-media mappings or `WP_HOME` overrides into
the general templates. Keep such custom behavior in project-owned configuration.
Existing database directories and volume identities are not migrated or deleted.

## Other commands

```sh
wpdev stop                        # down; preserves database volumes
wpdev rebuild                     # render/build/up; remove obsolete services
wpdev xdebug on                    # debug,develop; trigger-based, IDE port 9003
wpdev xdebug off
wpdev db dump                     # streamed, timestamped SQL backup
wpdev db import ./backup.sql       # input path is relative to the invoking shell
```

MariaDB uses `mariadb-dump`/`mariadb`; MySQL uses `mysqldump`/`mysql`. Dumps are
published only when the subprocess and file close succeed. Imports target the
local `wordpress` database; use dumps appropriate for the configured engine.

## Development and tests

```sh
go test ./...
go vet ./...
WPDEV_DOCKER_TESTS=1 go test ./internal/cli -run TestDockerBuild -v -timeout 60m
WPDEV_DOCKER_TESTS=1 go test ./internal/cli -run 'TestDockerRuntime|TestDockerMySQL|TestDockerServices' -v -timeout 60m
WPDEV_DOCKER_TESTS=1 go test ./internal/cli -run 'TestDockerImport|TestDockerConfigLocal' -v -timeout 30m
WPDEV_DOCKER_TESTS=1 go test ./internal/cli -run TestDockerPlugins -v -timeout 45m
WPDEV_DOCKER_TESTS=1 go test ./internal/cli -run TestDockerPullFiles -v -timeout 30m
```

Docker tests create isolated temporary projects, expose no host ports and remove
their own containers and database volumes. Test overrides use `!reset`, requiring
Compose v2.24.4+. Build tests cover PHP 8.3/8.4/8.5 with Apache/FPM. Runtime tests
cover single-site and both multisite modes on both servers, HTTP/HTTPS, login,
static/PHP routes, internal clients, certificate rejection, Redis, restart and
backup/restore. `WPDEV_WP_VERSION` can pin the WordPress download; its default is
`latest`. Integration images are cached as `wpdev-integration:*` to speed repeats.
