package cli

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed assets/templates/* assets/legacy/* assets/import/*
var assets embed.FS
var templateNames = []string{"docker-compose.tmpl.yml", "php.Dockerfile.tmpl", "nginx.conf.tmpl", "Caddyfile.http.tmpl", "Caddyfile.mkcert.tmpl", "entrypoint.sh.tmpl", "wpdev-local.php.tmpl", "wp-cli.yml.tmpl"}

type route struct{ Host, Upstream, Certificate string }
type renderData struct {
	*Config
	ContainerDocroot string
	Routes           []route
}
type templateState struct {
	Templates   map[string]string `json:"templates"`
	ComposeHash string            `json:"compose_hash"`
}

func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func normalized(b []byte) string {
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n"))
}
func readTemplateState(c *Config) (templateState, error) {
	s := templateState{Templates: map[string]string{}}
	b, err := os.ReadFile(c.file(".wpdev", "template-state.json"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	if s.Templates == nil {
		s.Templates = map[string]string{}
	}
	return s, err
}
func writeTemplateState(c *Config, s templateState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(c.file(".wpdev", "template-state.json"), b, 0o644)
}
func bundled(name string) []byte {
	b, err := assets.ReadFile("assets/templates/" + name)
	if err != nil {
		panic(err)
	}
	return b
}
func managedTemplate(name string, b []byte, s templateState) bool {
	if normalized(b) == normalized(bundled(name)) || s.Templates[name] == hash(b) {
		return true
	}
	old, err := assets.ReadFile("assets/legacy/" + name)
	return err == nil && normalized(old) == normalized(b)
}
func templateSource(c *Config, name string, s templateState, upgrade bool) ([]byte, error) {
	b, err := os.ReadFile(c.file(".wpdev", "templates", name))
	if os.IsNotExist(err) {
		return bundled(name), nil
	}
	if err != nil {
		return nil, err
	}
	if upgrade && managedTemplate(name, b, s) {
		return bundled(name), nil
	}
	return b, nil
}
func dataFor(c *Config) renderData {
	up := "php:80"
	if c.Web.Server != "apache" {
		up = "web:80"
	}
	d := renderData{Config: c, ContainerDocroot: c.containerDocroot(), Routes: []route{{c.Domain, up, c.Domain}}}
	if c.Multisite.Enabled && c.Multisite.Mode == "subdomain" {
		d.Routes = append(d.Routes, route{"*." + c.Domain, up, "_wildcard." + c.Domain})
	}
	if c.Services.Mailpit {
		d.Routes = append(d.Routes, route{"mail." + c.Domain, "mailpit:8025", "_wildcard." + c.Domain})
	}
	if c.Services.Adminer {
		d.Routes = append(d.Routes, route{"db." + c.Domain, "adminer:8080", "_wildcard." + c.Domain})
	}
	return d
}
func executeTemplate(name string, b []byte, d renderData) ([]byte, error) {
	f := template.FuncMap{"quote": strconv.Quote, "phpquote": func(s string) string {
		return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'") + "'"
	}}
	t, err := template.New(name).Option("missingkey=error").Funcs(f).Parse(string(b))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	err = t.Execute(&out, d)
	return out.Bytes(), err
}
func sameYAML(a, b []byte) bool {
	var x, y any
	return yaml.Unmarshal(a, &x) == nil && yaml.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
func checkComposeOwnership(c *Config, s templateState) error {
	b, err := os.ReadFile(c.file("docker-compose.yml"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.ComposeHash != "" {
		if hash(b) == s.ComposeHash {
			return nil
		}
	} else {
		src, err := templateSource(c, "docker-compose.tmpl.yml", s, false)
		if err != nil {
			return err
		}
		rendered, err := executeTemplate("compose", src, dataFor(c))
		if err == nil && sameYAML(b, rendered) {
			return nil
		}
	}
	return fmt.Errorf("docker-compose.yml has untracked or manual edits; move additions to docker-compose.override.yml, then use wpdev templates update --apply --reset-custom to back up and explicitly accept replacement")
}
func loadAliases(c *Config) error {
	c.Aliases = []string{c.Domain}
	if c.Services.Mailpit {
		c.Aliases = append(c.Aliases, "mail."+c.Domain)
	}
	if c.Services.Adminer {
		c.Aliases = append(c.Aliases, "db."+c.Domain)
	}
	if c.Multisite.Enabled && c.Multisite.Mode == "subdomain" {
		b, err := os.ReadFile(c.file(".wpdev", "sites.json"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		var hosts []string
		if len(b) > 0 {
			if err = json.Unmarshal(b, &hosts); err != nil {
				return err
			}
		}
		for _, h := range hosts {
			if h == c.Domain {
				continue
			}
			if err = validateSiteHost(c, h); err != nil {
				return err
			}
			c.Aliases = append(c.Aliases, h)
		}
	}
	sort.Strings(c.Aliases)
	c.Aliases = uniqueStrings(c.Aliases)
	return nil
}
func uniqueStrings(in []string) []string {
	out := []string{}
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// Render every file before touching the active stack. Compose validates the
// staged model using the real project directory, including the user's override.
func renderTemplates(c *Config) error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := loadAliases(c); err != nil {
		return err
	}
	_, err := os.Stat(c.file(c.Web.Docroot, "wp-load.php"))
	c.WordPress = err == nil
	s, err := readTemplateState(c)
	if err != nil {
		return err
	}
	if err = checkComposeOwnership(c, s); err != nil {
		return err
	}
	outputs := map[string][]byte{}
	sources := map[string][]byte{}
	targets := map[string]string{"php.Dockerfile.tmpl": "php.Dockerfile", "nginx.conf.tmpl": "nginx.conf", "entrypoint.sh.tmpl": "entrypoint.sh", "wpdev-local.php.tmpl": "wpdev-local.php", "wp-cli.yml.tmpl": "wp-cli.yml"}
	caddy := "Caddyfile.http.tmpl"
	if c.TLS.Enabled {
		caddy = "Caddyfile.mkcert.tmpl"
	}
	targets[caddy] = "Caddyfile"
	for _, name := range templateNames {
		b, e := templateSource(c, name, s, true)
		if e != nil {
			return e
		}
		sources[name] = b
	}
	d := dataFor(c)
	for name, target := range targets {
		b, e := executeTemplate(name, sources[name], d)
		if e != nil {
			return fmt.Errorf("render %s: %w", name, e)
		}
		outputs[target] = b
	}
	// Fingerprints recreate services when bind-mounted generated files or CA change.
	fingerprint := []byte{}
	keys := []string{}
	for k := range outputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fingerprint = append(fingerprint, outputs[k]...)
	}
	c.RuntimeFingerprint = hash(fingerprint)
	d = dataFor(c)
	comp, err := executeTemplate("docker-compose.tmpl.yml", sources["docker-compose.tmpl.yml"], d)
	if err != nil {
		return err
	}
	var model map[string]any
	if err = yaml.Unmarshal(comp, &model); err != nil {
		return err
	}
	if model["services"] == nil {
		return fmt.Errorf("compose template must define services")
	}
	stage, err := os.CreateTemp(c.Root, ".wpdev-compose-*.yml")
	if err != nil {
		return err
	}
	defer os.Remove(stage.Name())
	if _, err = stage.Write(comp); err != nil {
		stage.Close()
		return err
	}
	if err = stage.Close(); err != nil {
		return err
	}
	args := []string{"compose", "--project-directory", c.Root, "-f", stage.Name()}
	for _, n := range []string{"docker-compose.override.yml", "docker-compose.override.yaml"} {
		if _, e := os.Stat(c.file(n)); e == nil {
			args = append(args, "-f", c.file(n))
			break
		}
	}
	if err = process(c.Root, nil, &bytes.Buffer{}, "docker", append(args, "config", "--quiet")...); err != nil {
		return err
	}
	// Back up only the managed files that are about to be upgraded.
	for _, name := range templateNames {
		p := c.file(".wpdev", "templates", name)
		old, e := os.ReadFile(p)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil && !managedTemplate(name, old, s) {
			fmt.Fprintln(os.Stderr, "Custom template retained:", p, "(review with wpdev templates update)")
			continue
		}
		if e == nil && normalized(old) != normalized(sources[name]) {
			if err = backupFile(c, p); err != nil {
				return err
			}
		}
		if err = atomicWrite(p, sources[name], 0o644); err != nil {
			return err
		}
		s.Templates[name] = hash(sources[name])
	}
	if c.WordPress && c.TLS.Enabled {
		if err = ensureWordPressLoader(c); err != nil {
			return err
		}
	}

	for _, k := range keys {
		if err = atomicWrite(c.file(".wpdev", "generated", k), outputs[k], 0o644); err != nil {
			return err
		}
	}
	if err = atomicWrite(c.file("docker-compose.yml"), comp, 0o644); err != nil {
		return err
	}
	s.ComposeHash = hash(comp)
	return writeTemplateState(c, s)
}

var templatesCmd = &cobra.Command{Use: "templates", Short: "Review and update project template overrides"}

func init() {
	var apply, reset bool
	cmd := &cobra.Command{Use: "update", Short: "Show template differences; optionally apply updates", RunE: func(cmd *cobra.Command, args []string) error {
		if reset && !apply {
			return fmt.Errorf("--reset-custom requires --apply")
		}
		c, err := projectConfig()
		if err != nil {
			return err
		}
		s, err := readTemplateState(c)
		if err != nil {
			return err
		}
		// Record ownership before replacing legacy templates, otherwise the old
		// generated Compose would no longer match its newly updated source.
		if apply && s.ComposeHash == "" {
			if err = loadAliases(c); err != nil {
				return err
			}
			if checkComposeOwnership(c, s) == nil {
				b, e := os.ReadFile(c.file("docker-compose.yml"))
				if e == nil {
					s.ComposeHash = hash(b)
				} else if !os.IsNotExist(e) {
					return e
				}
			}
		}
		for _, name := range templateNames {
			p := c.file(".wpdev", "templates", name)
			old, e := os.ReadFile(p)
			if e != nil && !os.IsNotExist(e) {
				return e
			}
			fresh := bundled(name)
			if normalized(old) == normalized(fresh) {
				continue
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n--- %s (current)\n+++ %s (bundled)\n", name, name)
			for _, line := range strings.Split(string(old), "\n") {
				fmt.Fprintln(cmd.OutOrStdout(), "-"+line)
			}
			for _, line := range strings.Split(string(fresh), "\n") {
				fmt.Fprintln(cmd.OutOrStdout(), "+"+line)
			}
			if !apply {
				continue
			}
			if e == nil && !managedTemplate(name, old, s) && !reset {
				if err = atomicWrite(p+".new", fresh, 0o644); err != nil {
					return err
				}
				continue
			}
			if err = backupFile(c, p); err != nil {
				return err
			}
			if err = atomicWrite(p, fresh, 0o644); err != nil {
				return err
			}
			s.Templates[name] = hash(fresh)
		}
		if apply {
			if reset {
				p := c.file("docker-compose.yml")
				if err = backupFile(c, p); err != nil {
					return err
				}
				b, e := os.ReadFile(p)
				if e != nil && !os.IsNotExist(e) {
					return e
				}
				if e == nil {
					s.ComposeHash = hash(b)
				}
			}
			return writeTemplateState(c, s)
		}
		return nil
	}}
	cmd.Flags().BoolVar(&apply, "apply", false, "update managed templates; write customized replacements as .new")
	cmd.Flags().BoolVar(&reset, "reset-custom", false, "back up and replace custom templates and authorize Compose regeneration")
	templatesCmd.AddCommand(cmd)
}
