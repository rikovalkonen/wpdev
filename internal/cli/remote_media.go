package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type mediaSite struct {
	Home      string `json:"home"`
	SiteURL   string `json:"siteurl"`
	Uploads   string `json:"uploads"`
	LocalHome string `json:"local_home"`
}
type mediaMap struct {
	MainID int               `json:"main_id"`
	Sites  map[int]mediaSite `json:"sites"`
}

func readMediaMap(c *Config) (mediaMap, error) {
	m := mediaMap{MainID: 1, Sites: map[int]mediaSite{}}
	b, err := os.ReadFile(c.file(".wpdev", "remote-media.json"))
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("remote media map: %w", err)
	}
	for id, s := range m.Sites {
		if id < 1 {
			return m, fmt.Errorf("invalid remote media site ID")
		}
		for _, u := range []string{s.Home, s.SiteURL, s.Uploads, s.LocalHome} {
			if _, err = absoluteSiteURL(u); err != nil {
				return m, fmt.Errorf("remote media site %d: %w", id, err)
			}
		}
	}
	return m, nil
}

// Read source upload URLs before any URL replacement, with source network
// constants but the isolated database and empty wp-content mount.
func (s *importStage) sourceMedia(source []byte, meta importMetadata) (mediaMap, error) {
	m := mediaMap{MainID: meta.MainID, Sites: map[int]mediaSite{}}
	p := importPlan{Target: meta}
	transformed, err := transformConfig(s.config, source, p.configValues())
	if err != nil {
		return m, err
	}
	if err = os.WriteFile(filepath.Join(s.dir, "wp-config.php"), []byte(transformed.Source), 0o600); err != nil {
		return m, err
	}
	for _, site := range meta.Sites {
		var out bytes.Buffer
		if err = s.wp(&out, "--url="+site.Home, "eval", `echo json_encode(['uploads'=>wp_get_upload_dir()['baseurl']]);`); err != nil {
			return m, fmt.Errorf("read source media for site %d: %w", site.ID, err)
		}
		var data struct {
			Uploads string `json:"uploads"`
		}
		if err = json.Unmarshal(out.Bytes(), &data); err != nil {
			return m, err
		}
		if _, err = absoluteSiteURL(data.Uploads); err != nil {
			return m, fmt.Errorf("source uploads URL for site %d: %w", site.ID, err)
		}
		m.Sites[site.ID] = mediaSite{Home: site.Home, SiteURL: site.SiteURL, Uploads: data.Uploads}
	}
	return m, nil
}
func planMediaMap(c *Config, p *importPlan, m mediaMap) (mediaMap, error) {
	previous, err := readMediaMap(c)
	if err != nil {
		return m, err
	}
	for _, site := range p.Target.Sites {
		s := m.Sites[site.ID]
		// Re-localizing an already local DB must not replace its source with itself.
		if old, ok := previous.Sites[site.ID]; ok && strings.TrimRight(s.Home, "/") == strings.TrimRight(old.LocalHome, "/") {
			s = old
		}
		s.LocalHome = site.Home
		m.Sites[site.ID] = s
	}
	return m, nil
}
func writeMediaMap(c *Config, m mediaMap) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(c.file(".wpdev", "remote-media.json"), b, 0o600)
}

var mediaSetEnv = regexp.MustCompile(`(?im)^\s*SetEnv\s+(WP_ENV|REMOTE_MEDIA_URL|WP_HOME)\b[^\r\n]*`)

func checkMediaHTAccess(c *Config) error {
	// Apache SetEnv is SAPI state and may override PHP's putenv even after a
	// multisite context switch. Never silently claim this configuration works.
	if c.Web.Server != "apache" {
		return nil
	}
	for dir := c.file(c.Web.Docroot); ; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, ".htaccess")
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if match := mediaSetEnv.Find(b); len(match) > 0 {
			return fmt.Errorf("remote media conflicts with %s: %s; remove the WP_ENV, REMOTE_MEDIA_URL and WP_HOME SetEnv directives from local .htaccess so wpdev can supply site-specific values (the file was not changed)", p, strings.TrimSpace(string(match)))
		}
		if dir == c.Root || dir == filepath.Dir(dir) {
			break
		}
	}
	return nil
}
