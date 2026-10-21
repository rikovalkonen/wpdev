package cli

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Each import has its own Compose project and internal-only network. Neither
// SQL nor wp-config can access the live DB, host network, or writable source.
type importStage struct {
	dir     string
	config  *Config
	meta    importMetadata
	started bool
}

func (s *importStage) run(in io.Reader, out io.Writer, args ...string) error {
	a := []string{"compose", "-p", filepath.Base(s.dir), "-f", filepath.Join(s.dir, "compose.yml")}
	return process(s.dir, in, out, "docker", append(a, args...)...)
}
func (s *importStage) close() {
	if !s.started {
		os.RemoveAll(s.dir)
		return
	}
	if err := s.run(nil, io.Discard, "down", "--volumes", "--remove-orphans"); err != nil {
		fmt.Fprintln(os.Stderr, "Temporary import cleanup failed; run:", "docker compose -p", filepath.Base(s.dir), "-f", filepath.Join(s.dir, "compose.yml"), "down --volumes")
		return // Retain Compose metadata so cleanup remains possible.
	}
	os.RemoveAll(s.dir)
}
func newImportStage(c *Config, config []byte, sourceRoot ...string) (*importStage, error) {
	dir, err := os.MkdirTemp("", "wpdev-import-")
	if err != nil {
		return nil, err
	}
	s := &importStage{dir: dir, config: c}
	ok := false
	defer func() {
		if !ok {
			s.close()
		}
	}()
	// Keep importer PHP independent of project-specific Apache configuration.
	buildCfg := *c
	buildCfg.Web.Server = "nginx"
	dockerfile, err := executeTemplate("php.Dockerfile.tmpl", bundled("php.Dockerfile.tmpl"), dataFor(&buildCfg))
	if err != nil {
		return nil, err
	}
	for name, data := range map[string][]byte{
		"Dockerfile": dockerfile, ".wpdev/generated/entrypoint.sh": bundled("entrypoint.sh.tmpl"),
		"wp-config.php": config, "inspect.php": importAsset("inspect.php"), "patch.php": importAsset("patch.php"),
	} {
		if err := atomicWrite(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	if err = os.MkdirAll(filepath.Join(dir, "content"), 0o755); err != nil {
		return nil, err
	}
	image := "wpdev-import-php:" + c.Web.PHP + "-" + hash(dockerfile)[:12]
	bind := func(source, target string) map[string]any {
		return map[string]any{"type": "bind", "source": source, "target": target, "read_only": true}
	}
	enginePrefix, ping := "MARIADB", "mariadb-admin"
	if c.Database.Engine == "mysql" {
		enginePrefix, ping = "MYSQL", "mysqladmin"
	}
	project := bind(c.Root, "/var/www/html")
	if len(sourceRoot) > 0 && sourceRoot[0] != "" {
		// Full pulls validate against the downloaded core, even in an empty
		// project. Do not create mount points in the active project directory.
		project = bind(sourceRoot[0], c.containerDocroot())
	}
	model := map[string]any{
		"services": map[string]any{
			"db": map[string]any{
				"image":       c.Database.Engine + ":" + c.Database.Version,
				"environment": map[string]string{enginePrefix + "_ROOT_PASSWORD": "root", enginePrefix + "_DATABASE": "wordpress", enginePrefix + "_USER": "wp", enginePrefix + "_PASSWORD": "secret"},
				"volumes":     []string{"data:/var/lib/mysql"},
				"healthcheck": map[string]any{"test": []string{"CMD", ping, "ping", "-h", "127.0.0.1", "-uroot", "-proot"}, "interval": "2s", "timeout": "5s", "retries": 60},
			},
			"php": map[string]any{
				"image": image, "build": map[string]any{"context": dir, "args": map[string]string{"PHP_VERSION": c.Web.PHP}},
				"command":     []string{"sleep", "infinity"},
				"environment": map[string]string{"WPDEV_LOCAL": "1", "XDEBUG_MODE": "off", "WP_CLI_CACHE_DIR": "/tmp/wp-cli-cache", "WP_CLI_CONFIG_PATH": "/dev/null"},
				"volumes": []any{project, bind(filepath.Join(dir, "wp-config.php"), c.containerDocroot()+"/wp-config.php"),
					bind(filepath.Join(dir, "content"), c.containerDocroot()+"/wp-content"), bind(dir, "/wpdev-import")},
				"depends_on": map[string]any{"db": map[string]string{"condition": "service_healthy"}},
			},
		},
		"volumes":  map[string]any{"data": map[string]any{}},
		"networks": map[string]any{"default": map[string]any{"internal": true}},
	}
	b, err := yaml.Marshal(model)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(dir, "compose.yml"), b, 0o600); err != nil {
		return nil, err
	}
	s.started = true
	if err = s.run(nil, os.Stdout, "up", "-d", "--build", "--wait", "--wait-timeout", "180"); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

type sqlInput struct {
	io.Reader
	closers []io.Closer
}

func (s *sqlInput) Close() error {
	var err error
	for _, c := range s.closers {
		if e := c.Close(); err == nil {
			err = e
		}
	}
	return err
}
func openSQL(path string) (*sqlInput, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(f)
	head, _ := reader.Peek(2)
	if len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			f.Close()
			return nil, err
		}
		return &sqlInput{Reader: gz, closers: []io.Closer{gz, f}}, nil
	}
	return &sqlInput{Reader: reader, closers: []io.Closer{f}}, nil
}
func (s *importStage) importSQL(path string) error {
	f, err := openSQL(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, client := dbTools(s.config)
	// --binary-mode also disables mysql's interactive commands from dump input.
	return s.run(f, os.Stdout, "exec", "-T", "db", client, "--binary-mode=1", "--local-infile=0", "-uroot", "-proot", "wordpress")
}
func (s *importStage) script(name string, request, response any) error {
	b, err := json.Marshal(request)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err = s.run(bytes.NewReader(b), &out, "exec", "-T", "php", "php", "/wpdev-import/"+name); err != nil {
		return err
	}
	if response != nil {
		return json.Unmarshal(out.Bytes(), response)
	}
	return nil
}
func (s *importStage) inspect(opts importOptions, hint string) (importMetadata, error) {
	var meta importMetadata
	mode := opts.Mode
	if mode == "" {
		mode = hint
	}
	err := s.script("inspect.php", map[string]string{"prefix": opts.Prefix, "mode": mode}, &meta)
	if err == nil {
		s.meta = meta
	}
	return meta, err
}
func (s *importStage) wp(out io.Writer, args ...string) error {
	a := []string{"exec", "-T", "-e", "WPDEV_DB_NAME=" + s.meta.Schema, "php", "php", "-d", "error_reporting=E_ALL & ~E_DEPRECATED", "/usr/local/bin/wp",
		"--allow-root", "--path=" + s.config.containerDocroot(), "--skip-plugins", "--skip-themes", "--skip-packages", "--no-color"}
	return s.run(nil, out, append(a, args...)...)
}
func (s *importStage) localize(p *importPlan, source []byte) ([]byte, error) {
	transformed, err := transformConfig(s.config, source, p.configValues())
	if err != nil {
		return nil, err
	}
	// Docker's individual bind mount pins the inode: write in place in staging.
	if err = os.WriteFile(filepath.Join(s.dir, "wp-config.php"), []byte(transformed.Source), 0o600); err != nil {
		return nil, err
	}
	if err = s.script("patch.php", p.Target, nil); err != nil {
		return nil, err
	}
	urlArg := "--url=" + p.Target.Sites[0].Home
	for _, site := range p.Target.Sites {
		if site.ID == p.Target.MainID {
			urlArg = "--url=" + site.Home
		}
	}
	if err = s.wp(os.Stdout, urlArg, "core", "is-installed"); err != nil {
		return nil, fmt.Errorf("staged WordPress cannot load (custom config includes may need adjustment): %w", err)
	}
	// Two passes prevent chains/swaps of source and destination URLs from
	// rewriting an earlier replacement. Only URL prefixes, never bare domains.
	type replacement struct{ old, placeholder, target string }
	var replacements []replacement
	nonce := strings.TrimPrefix(filepath.Base(s.dir), "wpdev-import-")
	for _, pair := range p.Replacements {
		for _, escaped := range []bool{true, false} {
			old, target := pair.Old, pair.New
			if escaped {
				old = strings.ReplaceAll(old, "/", "\\/")
				target = strings.ReplaceAll(target, "/", "\\/")
			}
			placeholder := fmt.Sprintf("https://wpdev-%s-%d.invalid", nonce, len(replacements))
			if escaped {
				placeholder = strings.ReplaceAll(placeholder, "/", "\\/")
			}
			replacements = append(replacements, replacement{old, placeholder, target})
		}
	}
	for pass := 0; pass < 2; pass++ {
		for _, r := range replacements {
			old, next := r.old, r.placeholder
			if pass == 1 {
				old, next = r.placeholder, r.target
			}
			// Require a URL boundary after the host/path. Escaped JSON strings
			// use a backslash before their slash/quote delimiters.
			pattern := regexp.QuoteMeta(old) + `(?=$|[/\\?#\s"'<>])`
			if err = s.wp(os.Stdout, urlArg, "search-replace", pattern, next, "--regex", "--precise", "--all-tables-with-prefix", "--skip-columns=guid", "--report-changed-only"); err != nil {
				return nil, err
			}
		}
	}
	// Reassert canonical values after URL replacements, and validate WP's view.
	if err = s.script("patch.php", p.Target, nil); err != nil {
		return nil, err
	}
	for _, site := range p.Target.Sites {
		var out bytes.Buffer
		if err = s.wp(&out, "--url="+site.Home, "option", "get", "home"); err != nil {
			return nil, err
		}
		if strings.TrimSpace(out.String()) != site.Home {
			return nil, fmt.Errorf("site %d home did not localize", site.ID)
		}
	}
	return []byte(transformed.Source), nil
}
func (s *importStage) export(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	dump, _ := dbTools(s.config)
	args := []string{"exec", "-T", "db", dump, "-uroot", "-proot", "--single-transaction", "--skip-lock-tables"}
	if s.config.Database.Engine == "mysql" {
		args = append(args, "--set-gtid-purged=OFF", "--no-tablespaces")
	}
	// Preserve all supplied tables. URL changes are limited to the selected
	// prefix; SSH exports have already selected that installation's tables.
	// Never include CREATE/USE DATABASE in the published dump.
	args = append(args, "--", s.meta.Schema)
	args = append(args, s.meta.Tables...)
	err = s.run(nil, f, args...)
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}
