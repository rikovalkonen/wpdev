package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"text/template"
)

func renderTemplates(cfg *Config) error {
	// Load templates from .wpdev/templates
	tplDir := filepath.Join(".wpdev", "templates")
	content, err := os.ReadFile(filepath.Join(tplDir, "nginx.conf.tmpl"))
	if err != nil { return err }
	nginxTpl, err := template.New("nginx").Parse(string(content))
	if err != nil { return err }

	var out bytes.Buffer
	if err := nginxTpl.Execute(&out, cfg); err != nil { return err }
	_ = os.MkdirAll(filepath.Join(".wpdev", "generated"), 0o755)
	if err := os.WriteFile(filepath.Join(".wpdev", "generated", "nginx.conf"), out.Bytes(), 0o644); err != nil { return err }

	// PHP Dockerfile
    content, err = os.ReadFile(filepath.Join(tplDir, "php.Dockerfile.tmpl"))
    if err != nil { return err }
    phpTpl, err := template.New("dockerfile").Parse(string(content))
    if err != nil { return err }
    out.Reset()
    if err := phpTpl.Execute(&out, cfg); err != nil { return err }
    if err := os.WriteFile(filepath.Join(".wpdev", "generated", "php.Dockerfile"), out.Bytes(), 0o644); err != nil { return err }

	// docker-compose
	content, err = os.ReadFile(filepath.Join(tplDir, "docker-compose.tmpl.yml"))
	if err != nil { return err }
	composeTpl, err := template.New("compose").Parse(string(content))
	if err != nil { return err }
	out.Reset()
	if err := composeTpl.Execute(&out, cfg); err != nil { return err }
	if err := os.WriteFile("docker-compose.yml", out.Bytes(), 0o644); err != nil { return err }

	tplName := "Caddyfile.http.tmpl"
	if cfg.TLS.Enabled {
		tplName = "Caddyfile.mkcert.tmpl"
	}

	content, err = os.ReadFile(filepath.Join(".wpdev", "templates", tplName))
	if err != nil { return err }

	caddyTpl, err := template.New("caddy").Parse(string(content))
	if err != nil { return err }

	out.Reset()
	if err := caddyTpl.Execute(&out, cfg); err != nil { return err }
	if err := os.WriteFile(filepath.Join(".wpdev", "generated", "Caddyfile"), out.Bytes(), 0o644); err != nil { return err }

	return nil
}
