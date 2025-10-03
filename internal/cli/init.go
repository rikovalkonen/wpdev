package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a wpdev project (interactive)",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Welcome to wpdev init — press ENTER to accept defaults.")

		name := prompt("Project name", "mysite")
		domain := prompt("Domain", name+".test")
		php := prompt("PHP version", "8.3")
    server := strings.ToLower(prompt("Web server (apache/nginx)", "apache"))
    if server != "apache" && server != "nginx" {
        server = "apache"
    }
		docroot := prompt("Document root", "wp")

		dbEngine := strings.ToLower(prompt("Database engine (mariadb/mysql)", "mariadb"))
		dbVersion := prompt("Database version", "11.4")

    persist := strings.ToLower(prompt("Persist DB data as bind mount? (bind/volume)", "bind"))
    if persist != "bind" && persist != "volume" {
        persist = "bind"
    }
    
    tlsAns := strings.ToLower(prompt("Enable TLS (mkcert)? (y/n)", "y"))

		redisAns := strings.ToLower(prompt("Enable Redis? (y/n)", "y"))
		mailpitAns := strings.ToLower(prompt("Enable Mailpit? (y/n)", "y"))
		adminerAns := strings.ToLower(prompt("Enable Adminer? (y/n)", "y"))

		cfg := &Config{
			Name:   name,
			Domain: domain,
			Web:    WebCfg{Server: server, PHP: php, Docroot: docroot},
			Database: DBCfg{
				Engine:      dbEngine,
				Version:     dbVersion,
				Portforward: "3307",
			},
		}
		cfg.Services.Redis = redisAns == "y" || redisAns == "yes"
		cfg.Services.Mailpit = mailpitAns == "y" || mailpitAns == "yes"
		cfg.Services.Adminer = adminerAns == "y" || adminerAns == "yes"
		cfg.TLS.Enabled = tlsAns == "y" || tlsAns == "yes"
		cfg.Xdebug.Enabled = false
		cfg.Perf.Sync = "bind"
		cfg.Perf.Excludes = []string{"node_modules", "vendor", ".git"}
    cfg.Database.Persist = persist
    if cfg.Database.Persist == "bind" {
        dp := prompt("DB data folder (relative to project root)", "database")
        if dp == "" { dp = "database" }
        cfg.Database.DataPath = dp
    }
		// Write .wpdev.yml (ask before overwriting)
		if _, err := os.Stat(".wpdev.yml"); err == nil {
			ans := strings.ToLower(prompt(".wpdev.yml exists. Overwrite? (y/n)", "n"))
			if ans == "y" || ans == "yes" {
				if err := saveConfig(".wpdev.yml", cfg); err != nil {
					return err
				}
				fmt.Println("Wrote .wpdev.yml")
			} else {
				fmt.Println("Skipping .wpdev.yml overwrite.")
			}
		} else {
			if err := saveConfig(".wpdev.yml", cfg); err != nil {
				return err
			}
			fmt.Println("Wrote .wpdev.yml")
		}

    if cfg.Database.Persist == "bind" && cfg.Database.DataPath != "" {
        _ = os.MkdirAll(cfg.Database.DataPath, 0o755)
    }

		// Ensure template dir
		tplDir := filepath.Join(".wpdev", "templates")
		if err := os.MkdirAll(tplDir, 0o755); err != nil {
			return err
		}

		// Write default templates if missing
		writeIfMissing(filepath.Join(tplDir, "docker-compose.tmpl.yml"), []byte(dockerComposeTemplate))
		writeIfMissing(filepath.Join(tplDir, "nginx.conf.tmpl"), []byte(nginxConfTemplate))
		writeIfMissing(filepath.Join(tplDir, "php.Dockerfile.tmpl"), []byte(phpDockerfileTemplate))
		writeIfMissing(filepath.Join(tplDir, "Caddyfile.mkcert.tmpl"), []byte(caddyfileMkcertTemplate))
    writeIfMissing(filepath.Join(tplDir, "Caddyfile.http.tmpl"), []byte(caddyfileHttpTemplate))


		// Bootstrap a simple index.php if docroot is empty
		if cfg.Web.Docroot == "" {
			cfg.Web.Docroot = "."
		}
		indexPath := filepath.Join(cfg.Web.Docroot, "index.php")
		if _, err := os.Stat(indexPath); os.IsNotExist(err) {
			_ = os.MkdirAll(cfg.Web.Docroot, 0o755)
			_ = os.WriteFile(indexPath, []byte("<?php phpinfo();"), 0o644)
			fmt.Printf("Wrote %s\n", indexPath)
		}

    if cfg.TLS.Enabled && !certsPresent(cfg.Domain) {
        fmt.Println("TLS enabled: generating mkcert certificates...")
        if err := generateCertsForDomain(cfg.Domain); err != nil {
            fmt.Printf("Warning: TLS certificate generation failed: %v\n", err)
            fmt.Println("You can retry later with: wpdev tls init")
        } else {
            fmt.Println("TLS certificates created.")
        }
    }

    fmt.Println("Init complete. Next: `wpdev start`")

		
		return nil
	},
}

func prompt(label, def string) string {
	fmt.Printf("%s [%s]: ", label, def)
	reader := bufio.NewReader(os.Stdin)
	text, _ := reader.ReadString('\n')
	text = strings.TrimSpace(text)
	if text == "" {
		return def
	}
	return text
}

func writeIfMissing(path string, content []byte) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		_ = os.WriteFile(path, content, 0o644)
	}
}

// ===== Templates (same content as before) =====

const dockerComposeTemplate = `
services:
  php:
    build:
      context: .
      dockerfile: .wpdev/generated/php.Dockerfile
      args:
        PHP_VERSION: {{ .Web.PHP }}
    volumes:
      - ./:/var/www/html:delegated
    environment:
      - XDEBUG_MODE={{ if .Xdebug.Enabled }}debug,develop{{ else }}off{{ end }}
      - PHP_IDE_CONFIG=serverName=wpdev
    depends_on:
      - db
{{- if ne .Web.Server "apache" }}

  web:
    image: nginx:stable
    volumes:
      - ./:/var/www/html:delegated
      - ./.wpdev/generated/nginx.conf:/etc/nginx/conf.d/default.conf
    depends_on:
      - php
{{- end }}

  db:
    image: {{ if eq .Database.Engine "mysql" }}mysql:{{ .Database.Version }}{{ else }}mariadb:{{ .Database.Version }}{{ end }}
    environment:
      - {{ if eq .Database.Engine "mysql" }}MYSQL_DATABASE{{ else }}MARIADB_DATABASE{{ end }}=wordpress
      - {{ if eq .Database.Engine "mysql" }}MYSQL_USER{{ else }}MARIADB_USER{{ end }}=wp
      - {{ if eq .Database.Engine "mysql" }}MYSQL_PASSWORD{{ else }}MARIADB_PASSWORD{{ end }}=secret
      - {{ if eq .Database.Engine "mysql" }}MYSQL_ROOT_PASSWORD{{ else }}MARIADB_ROOT_PASSWORD{{ end }}=root
    volumes:
{{- if eq .Database.Persist "bind" }}
      - ./{{ .Database.DataPath }}:/var/lib/mysql
{{- else }}
      - dbdata:/var/lib/mysql
{{- end }}
    ports:
      - "{{ .Database.Portforward }}:3306"

{{- if .Services.Mailpit }}
  mailpit:
    image: axllent/mailpit
{{- end }}

{{- if .Services.Adminer }}
  adminer:
    image: adminer:latest
    depends_on:
      - db
{{- end }}

  caddy:
    image: caddy:2
    depends_on:
{{- if eq .Web.Server "apache" }}
      - php
{{- else }}
      - web
{{- end }}
{{- if .Services.Mailpit }}
      - mailpit
{{- end }}
{{- if .Services.Adminer }}
      - adminer
{{- end }}
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./.wpdev/generated/Caddyfile:/etc/caddy/Caddyfile:ro
      - ./.wpdev/certs:/certs:ro

{{- if ne .Database.Persist "bind" }}
volumes:
  dbdata: {}
{{- end }}
`

const nginxConfTemplate = `server {
  listen 80;
  server_name _;
  root /var/www/html/{{ .Web.Docroot }};
  index index.php index.html;

  client_max_body_size 64m;

  location / {
    try_files $uri $uri/ /index.php?$args;
  }

  location ~ \\.php$ {
    include fastcgi_params;
    fastcgi_pass php:9000;
    fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
    fastcgi_buffers 16 16k;
    fastcgi_index index.php;
  }
}
`

const phpDockerfileTemplate = `ARG PHP_VERSION
{{- if eq .Web.Server "apache" }}
FROM php:${PHP_VERSION}-apache
{{- else }}
FROM php:${PHP_VERSION}-fpm
{{- end }}

# ── deps for common WP extensions ───────────────────────────────────────────────
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ghostscript \
      libavif-dev libfreetype6-dev libicu-dev libjpeg-dev libpng-dev libwebp-dev \
      libzip-dev libmagickwand-dev libmagickcore-7.q16-10 libzip5 \
      mariadb-client git unzip; \
    rm -rf /var/lib/apt/lists/*

# ── PHP extensions (WordPress recommendations) ──────────────────────────────────
RUN set -eux; \
    docker-php-ext-configure gd --with-avif --with-freetype --with-jpeg --with-webp; \
    docker-php-ext-install -j"$(nproc)" bcmath exif gd intl mysqli soap zip; \
    pecl install imagick-3.8.0; docker-php-ext-enable imagick

# ── Opcache + sane dev logging ─────────────────────────────────────────────────
RUN set -eux; \
    docker-php-ext-enable opcache; \
    { \
      echo 'opcache.memory_consumption=128'; \
      echo 'opcache.interned_strings_buffer=8'; \
      echo 'opcache.max_accelerated_files=8000'; \
      echo 'opcache.revalidate_freq=2'; \
    } > /usr/local/etc/php/conf.d/opcache-recommended.ini; \
    { \
      echo 'error_reporting = E_ALL & ~E_DEPRECATED & ~E_STRICT'; \
      echo 'display_errors = Off'; \
      echo 'log_errors = On'; \
      echo 'error_log = /dev/stderr'; \
    } > /usr/local/etc/php/conf.d/error-logging.ini

{{- if eq .Web.Server "apache" }}
# ── Apache behind Caddy (NO SSL here; Caddy terminates TLS) ────────────────────
RUN set -eux; \
    a2enmod rewrite expires remoteip; \
    { \
      echo 'RemoteIPHeader X-Forwarded-For'; \
      echo 'RemoteIPTrustedProxy 10.0.0.0/8'; \
      echo 'RemoteIPTrustedProxy 172.16.0.0/12'; \
      echo 'RemoteIPTrustedProxy 192.168.0.0/16'; \
      echo 'RemoteIPTrustedProxy 127.0.0.0/8'; \
    } > /etc/apache2/conf-available/remoteip.conf; \
    a2enconf remoteip; \
    echo "ServerName localhost" >> /etc/apache2/apache2.conf

# Optionally set docroot to {{ .Web.Docroot }}
{{- if and (ne .Web.Docroot ".") (ne .Web.Docroot "") }}
RUN set -eux; \
    sed -ri 's#DocumentRoot /var/www/html#DocumentRoot /var/www/html/{{ .Web.Docroot }}#' /etc/apache2/sites-available/000-default.conf; \
    printf '<Directory /var/www/html/{{ .Web.Docroot }}>\nAllowOverride All\nRequire all granted\n</Directory>\n' \
      > /etc/apache2/conf-available/docroot.conf; \
    a2enconf docroot
{{- end }}
{{- end }}

WORKDIR /var/www/html
`

const caddyfileMkcertTemplate = `
{{ $domain := .Domain }}
{{ $up := "php:80" }}{{ if ne .Web.Server "apache" }}{{ $up = "web:80" }}{{ end }}

https://{{$domain}} {
  encode gzip
  log
  tls /certs/{{$domain}}.pem /certs/{{$domain}}-key.pem
  reverse_proxy {{$up}} {
    header_up X-Forwarded-Proto https
    header_up X-Forwarded-Host {host}
    header_up X-Real-IP {remote_host}
  }
}
http://{{$domain}} {
  redir https://{{$domain}}{uri} 308
}

{{ if .Services.Mailpit }}
https://mail.{{$domain}} {
  encode gzip
  log
  tls /certs/_wildcard.{{$domain}}.pem /certs/_wildcard.{{$domain}}-key.pem
  reverse_proxy mailpit:8025
}
http://mail.{{$domain}} {
  redir https://mail.{{$domain}}{uri} 308
}
{{ end }}

{{ if .Services.Adminer }}
https://db.{{$domain}} {
  encode gzip
  log
  tls /certs/_wildcard.{{$domain}}.pem /certs/_wildcard.{{$domain}}-key.pem
  reverse_proxy adminer:8080
}
http://db.{{$domain}} {
  redir https://db.{{$domain}}{uri} 308
}
{{ end }}
`

const caddyfileHttpTemplate = `
{{ $domain := .Domain }}
{{ $up := "php:80" }}{{ if ne .Web.Server "apache" }}{{ $up = "web:80" }}{{ end }}

http://{{$domain}} {
  encode gzip
  log
  reverse_proxy {{$up}}
}

{{ if .Services.Mailpit }}
http://mail.{{$domain}} {
  encode gzip
  log
  reverse_proxy mailpit:8025
}
{{ end }}

{{ if .Services.Adminer }}
http://db.{{$domain}} {
  encode gzip
  log
  reverse_proxy adminer:8080
}
{{ end }}
`