package cli

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Name     string `yaml:"name"`
	Domain   string `yaml:"domain"`
	Web      WebCfg `yaml:"web"`
	Database DBCfg  `yaml:"database"`
	Services struct {
		Redis   bool `yaml:"redis"`
		Mailpit bool `yaml:"mailpit"`
		Adminer bool `yaml:"adminer"`
	} `yaml:"services"`
	Xdebug struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"xdebug"`
	Perf struct {
		Sync     string   `yaml:"sync"`
		Excludes []string `yaml:"excludes"`
	} `yaml:"perf"`
	TLS                TLSCfg            `yaml:"tls"`
	Multisite          MultisiteCfg      `yaml:"multisite"`
	SSH                map[string]SSHCfg `yaml:"ssh,omitempty"`
	Root               string            `yaml:"-"`
	ConfigPath         string            `yaml:"-"`
	Aliases            []string          `yaml:"-"`
	TLSFingerprint     string            `yaml:"-"`
	RuntimeFingerprint string            `yaml:"-"`
	WordPress          bool              `yaml:"-"`
	document           *yaml.Node
}
type SSHCfg struct {
	Host   string `yaml:"host"`
	User   string `yaml:"user,omitempty"`
	Port   int    `yaml:"port,omitempty"`
	WPPath string `yaml:"wp_path"`
}
type WebCfg struct {
	Server  string `yaml:"server"`
	PHP     string `yaml:"php"`
	Docroot string `yaml:"docroot"`
}
type DBCfg struct {
	Engine      string `yaml:"engine"`
	Version     string `yaml:"version"`
	Portforward string `yaml:"portforward"`
	Persist     string `yaml:"persist"`
	DataPath    string `yaml:"data_path"`
}
type TLSCfg struct {
	Enabled bool `yaml:"enabled"`
}
type MultisiteCfg struct {
	Enabled  bool   `yaml:"enabled"`
	Mode     string `yaml:"mode"`
	BasePath string `yaml:"base_path"`
}

func defaultConfig() *Config {
	c := &Config{Web: WebCfg{Server: "apache", PHP: "8.5", Docroot: "wp"}, Database: DBCfg{Engine: "mariadb", Version: "11.4", Portforward: "3307", Persist: "volume"}, Multisite: MultisiteCfg{Mode: "subdirectory", BasePath: "/"}}
	c.Perf.Sync = "bind"
	return c
}
func configPath() (string, error) {
	p := cfgFile
	if p == "" {
		p = ".wpdev.yml"
	}
	return filepath.Abs(p)
}
func projectConfig() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return nil, err
	}
	return loadConfig(p)
}
func loadConfig(p string) (*Config, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w (run wpdev init for a new project)", p, err)
	}
	c := defaultConfig()
	var doc yaml.Node
	if err = yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must contain a YAML mapping", p)
	}
	// Older configs with an omitted persistence mode used the named dbdata volume.
	if err = doc.Decode(c); err != nil {
		return nil, err
	}
	if c.Database.Engine == "mysql" && !hasYAMLKey(doc.Content[0], "database", "version") {
		c.Database.Version = "8.4"
	}
	c.ConfigPath, err = filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	c.Root = filepath.Dir(c.ConfigPath)
	c.document = &doc
	if err = c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return c, nil
}
func hasYAMLKey(n *yaml.Node, keys ...string) bool {
	if len(keys) == 0 {
		return true
	}
	if n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == keys[0] {
			return hasYAMLKey(n.Content[i+1], keys[1:]...)
		}
	}
	return false
}
func mergeYAML(dst, src *yaml.Node) {
	if dst.Kind == yaml.MappingNode && src.Kind == yaml.MappingNode {
		for i := 0; i < len(src.Content); i += 2 {
			found := false
			for j := 0; j < len(dst.Content); j += 2 {
				if dst.Content[j].Value == src.Content[i].Value {
					mergeYAML(dst.Content[j+1], src.Content[i+1])
					found = true
					break
				}
			}
			if !found {
				dst.Content = append(dst.Content, src.Content[i], src.Content[i+1])
			}
		}
		return
	}
	head, line, foot := dst.HeadComment, dst.LineComment, dst.FootComment
	*dst = *src
	dst.HeadComment, dst.LineComment, dst.FootComment = head, line, foot
}
func saveConfig(p string, c *Config) error {
	if err := c.validate(); err != nil {
		return err
	}
	var src yaml.Node
	if err := src.Encode(c); err != nil {
		return err
	}
	dst := c.document
	if dst == nil {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			var existing yaml.Node
			if err = yaml.Unmarshal(b, &existing); err != nil {
				return err
			}
			if len(existing.Content) != 1 || existing.Content[0].Kind != yaml.MappingNode {
				return fmt.Errorf("existing config must be a YAML mapping")
			}
			dst = &existing
		}
	}
	if dst == nil {
		dst = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{&src}}
	} else {
		mergeYAML(dst.Content[0], &src)
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(dst); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(p); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := atomicWrite(p, b.Bytes(), mode); err != nil {
		return err
	}
	c.document = dst
	return nil
}

var hostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
var safePathPattern = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)
var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(?:\.[0-9]+)?$`)

func validHost(s string) bool {
	if len(s) > 253 || net.ParseIP(s) != nil || !strings.Contains(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) > 63 || !hostPattern.MatchString(label) {
			return false
		}
	}
	return true
}
func relativePath(p string) bool {
	return p != "" && safePathPattern.MatchString(p) && !filepath.IsAbs(p) && filepath.Clean(p) != ".." && !strings.HasPrefix(filepath.Clean(p), "../")
}
func (c *Config) validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	c.Domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(c.Domain), "."))
	if !validHost(c.Domain) {
		return fmt.Errorf("domain must be a DNS hostname, e.g. mysite.test")
	}
	if c.Web.Server != "apache" && c.Web.Server != "nginx" {
		return fmt.Errorf("web.server must be apache or nginx")
	}
	if !versionPattern.MatchString(c.Web.PHP) {
		return fmt.Errorf("web.php must be a numeric PHP version")
	}
	if !relativePath(c.Web.Docroot) {
		return fmt.Errorf("web.docroot must be a relative path using letters, numbers, /, _, - or .")
	}
	c.Web.Docroot = filepath.ToSlash(filepath.Clean(c.Web.Docroot))
	if c.Database.Engine != "mariadb" && c.Database.Engine != "mysql" {
		return fmt.Errorf("database.engine must be mariadb or mysql")
	}
	if !imageTagPattern.MatchString(c.Database.Version) {
		return fmt.Errorf("database.version must be a Docker image tag")
	}
	if c.Database.Portforward != "" {
		n, err := strconv.Atoi(c.Database.Portforward)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("database.portforward must be empty or a port between 1 and 65535")
		}
	}
	if c.Database.Persist != "bind" && c.Database.Persist != "volume" {
		return fmt.Errorf("database.persist must be bind or volume")
	}
	if c.Database.Persist == "bind" && !relativePath(c.Database.DataPath) {
		return fmt.Errorf("database.data_path must be a relative path")
	}
	if c.Multisite.Mode != "subdirectory" && c.Multisite.Mode != "subdomain" {
		return fmt.Errorf("multisite.mode must be subdirectory or subdomain")
	}
	base := c.Multisite.BasePath
	if !strings.HasPrefix(base, "/") || !strings.HasSuffix(base, "/") || !safePathPattern.MatchString(base) || strings.Contains(base, "//") || strings.Contains(base, "..") || path.Clean(base)+"/" != base && base != "/" {
		return fmt.Errorf("multisite.base_path must be a normalized URL path starting and ending with /, e.g. /")
	}
	return nil
}
func (c *Config) file(parts ...string) string {
	return filepath.Join(append([]string{c.Root}, parts...)...)
}
func (c *Config) containerDocroot() string { return path.Join("/var/www/html", c.Web.Docroot) }
func (c *Config) scheme() string {
	if c.TLS.Enabled {
		return "https"
	}
	return "http"
}
func (c *Config) URL() string {
	base := "/"
	if c.Multisite.Enabled {
		base = c.Multisite.BasePath
	}
	return c.scheme() + "://" + c.Domain + base
}
