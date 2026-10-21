package cli

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type importOptions struct {
	DryRun, Yes       bool
	Map, Prefix, Mode string
}
type importSite struct {
	ID      int    `json:"id"`
	Domain  string `json:"domain"`
	Path    string `json:"path"`
	Home    string `json:"home"`
	SiteURL string `json:"siteurl"`
}
type importMetadata struct {
	Schema       string       `json:"schema"`
	Prefix       string       `json:"prefix"`
	Multisite    bool         `json:"multisite"`
	Mode         string       `json:"mode"`
	Domain       string       `json:"domain"`
	BasePath     string       `json:"base_path"`
	NetworkID    int          `json:"network_id"`
	MainID       int          `json:"main_id"`
	Sites        []importSite `json:"sites"`
	Tables       []string     `json:"tables"`
	NoPrimaryKey []string     `json:"no_primary_key"`
}
type urlReplacement struct{ Old, New string }
type importPlan struct {
	Source       importMetadata
	Target       importMetadata
	Config       Config
	Replacements []urlReplacement
	Media        mediaMap
}

var sqlIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

func absoluteSiteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || !validHost(u.Hostname()) ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" || strings.Contains(u.Path, "..") || strings.Contains(u.Path, "//") {
		return nil, fmt.Errorf("invalid site URL %q (expected an HTTP(S) hostname and normalized path)", raw)
	}
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

func makeImportPlan(c *Config, m importMetadata, opts importOptions) (*importPlan, error) {
	if !sqlIdentifier.MatchString(m.Prefix) || len(m.Sites) == 0 {
		return nil, fmt.Errorf("invalid WordPress metadata")
	}
	if len(m.NoPrimaryKey) > 0 {
		return nil, fmt.Errorf("WP-CLI cannot safely replace URLs in tables without primary keys: %s; add keys or migrate these tables manually", strings.Join(m.NoPrimaryKey, ", "))
	}
	p := &importPlan{Source: m, Target: m, Config: *c}
	// saveConfig will load/merge the original document at publication time.
	// A staged plan must not share a mutable YAML tree with the live config.
	p.Config.document = nil
	p.Target.Sites = append([]importSite(nil), m.Sites...)
	p.Config.Multisite.Enabled = m.Multisite
	p.Config.Multisite.BasePath = m.BasePath
	if m.Multisite {
		p.Config.Multisite.Mode = m.Mode
	} else {
		// wpdev's document root already contains WordPress core.
		p.Config.Multisite.BasePath = "/"
		p.Target.BasePath = "/"
	}
	if err := p.Config.validate(); err != nil {
		return nil, err
	}
	p.Target.Domain = c.Domain
	var mappings struct {
		Sites map[int]string `yaml:"sites"`
	}
	if opts.Map != "" {
		b, err := os.ReadFile(opts.Map)
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err = dec.Decode(&mappings); err != nil {
			return nil, fmt.Errorf("URL map: %w", err)
		}
	}
	seen := map[string]int{}
	pairs := map[string]string{}
	addPair := func(old, target string) error {
		old, target = strings.TrimRight(old, "/"), strings.TrimRight(target, "/")
		if old == target {
			return nil
		}
		if previous, ok := pairs[old]; ok && previous != target {
			return fmt.Errorf("source URL %s maps to multiple destinations", old)
		}
		pairs[old] = target
		return nil
	}
	for i, s := range m.Sites {
		home, err := absoluteSiteURL(s.Home)
		if err != nil {
			return nil, err
		}
		siteURL, err := absoluteSiteURL(s.SiteURL)
		if err != nil {
			return nil, err
		}
		target := *home
		target.Scheme = c.scheme()
		target.Host = c.Domain
		target.Path = s.Path
		if !m.Multisite {
			target.Path = "/"
		}
		explicit, mapped := mappings.Sites[s.ID]
		if mapped {
			u, err := absoluteSiteURL(explicit)
			if err != nil {
				return nil, err
			}
			target = *u
			delete(mappings.Sites, s.ID)
		} else if s.Domain != m.Domain {
			if m.Mode != "subdomain" || !strings.HasSuffix(s.Domain, "."+m.Domain) {
				return nil, fmt.Errorf("site %d (%s) needs an explicit --map destination", s.ID, s.Home)
			}
			target.Host = strings.TrimSuffix(s.Domain, m.Domain) + c.Domain
		}
		if target.Scheme != c.scheme() {
			return nil, fmt.Errorf("site %d destination must use %s", s.ID, c.scheme())
		}
		if err := validateSiteHost(&p.Config, target.Host); err != nil {
			return nil, err
		}
		if (!m.Multisite || m.Mode == "subdirectory") && target.Host != c.Domain {
			return nil, fmt.Errorf("site %d must use the project domain in a subdirectory network", s.ID)
		}
		target.Path = strings.TrimRight(target.Path, "/") + "/"
		if !strings.HasPrefix(target.Path, p.Target.BasePath) || !safePathPattern.MatchString(target.Path) {
			return nil, fmt.Errorf("site %d destination is outside network base %s", s.ID, p.Target.BasePath)
		}
		if s.ID == m.MainID && (target.Host != c.Domain || target.Path != p.Target.BasePath) {
			return nil, fmt.Errorf("the main site must remain at %s://%s%s", c.scheme(), c.Domain, p.Target.BasePath)
		}
		key := target.Host + target.Path
		if id, ok := seen[key]; ok {
			return nil, fmt.Errorf("sites %d and %d share destination %s", id, s.ID, key)
		}
		seen[key] = s.ID
		// A production core directory (home=/, siteurl=/wp) is served directly
		// from web.docroot locally, so both URLs point to the local site root.
		homePath := strings.TrimRight(home.Path, "/")
		corePath := strings.TrimRight(siteURL.Path, "/")
		if siteURL.Host != home.Host || (corePath != homePath && !strings.HasPrefix(corePath, homePath+"/")) {
			return nil, fmt.Errorf("site %d has an unsupported home/siteurl relationship", s.ID)
		}
		newHome := strings.TrimRight(target.String(), "/")
		newCore := newHome
		p.Target.Sites[i] = importSite{ID: s.ID, Domain: target.Host, Path: target.Path, Home: newHome, SiteURL: newCore}
		for _, pair := range []urlReplacement{{s.Home, newHome}, {s.SiteURL, newCore}} {
			u, _ := absoluteSiteURL(pair.Old)
			for _, scheme := range []string{"https", "http", ""} {
				old := scheme + "://" + u.Host + strings.TrimRight(u.Path, "/")
				if scheme == "" {
					old = "//" + u.Host + strings.TrimRight(u.Path, "/")
				}
				dest := pair.New
				if scheme == "" {
					dest = strings.TrimPrefix(dest, c.scheme()+":")
				}
				if err := addPair(old, dest); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(mappings.Sites) > 0 {
		return nil, fmt.Errorf("URL map contains unknown site IDs")
	}
	for old, dest := range pairs {
		p.Replacements = append(p.Replacements, urlReplacement{old, dest})
	}
	sort.Slice(p.Replacements, func(i, j int) bool {
		if len(p.Replacements[i].Old) != len(p.Replacements[j].Old) {
			return len(p.Replacements[i].Old) > len(p.Replacements[j].Old)
		}
		return p.Replacements[i].Old < p.Replacements[j].Old
	})
	return p, nil
}

func (p *importPlan) configValues() map[string]any {
	v := localDBValues()
	v["table_prefix"] = p.Target.Prefix
	v["MULTISITE"] = p.Target.Multisite
	v["WP_ALLOW_MULTISITE"] = p.Target.Multisite
	v["WP_ENVIRONMENT_TYPE"] = "local"
	if p.Target.Multisite {
		v["SUBDOMAIN_INSTALL"] = p.Target.Mode == "subdomain"
		v["DOMAIN_CURRENT_SITE"] = p.Target.Domain
		v["PATH_CURRENT_SITE"] = p.Target.BasePath
		v["SITE_ID_CURRENT_SITE"] = p.Target.NetworkID
		v["BLOG_ID_CURRENT_SITE"] = p.Target.MainID
		v["WP_HOME"], v["WP_SITEURL"], v["COOKIE_DOMAIN"] = nil, nil, nil
	} else {
		v["WP_HOME"], v["WP_SITEURL"] = p.Target.Sites[0].Home, p.Target.Sites[0].SiteURL
		v["COOKIE_DOMAIN"] = nil
		// Old local multisite settings must not override a newly imported single site.
		for _, name := range []string{"SUBDOMAIN_INSTALL", "DOMAIN_CURRENT_SITE", "PATH_CURRENT_SITE", "SITE_ID_CURRENT_SITE", "BLOG_ID_CURRENT_SITE"} {
			v[name] = nil
		}
	}
	return v
}
