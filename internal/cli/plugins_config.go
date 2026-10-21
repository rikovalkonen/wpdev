package cli

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type LocalPluginsCfg struct {
	Enabled  *bool    `yaml:"enabled,omitempty"`
	Disabled []string `yaml:"disabled,omitempty"`
}
type RemoteMediaCfg struct {
	Enabled *bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	URL     string         `yaml:"url,omitempty" json:"url,omitempty"`
	Sites   map[int]string `yaml:"sites,omitempty" json:"sites,omitempty"`
}
type pluginSpec struct {
	Source      string `yaml:"source" json:"source"`
	Integration string `yaml:"integration,omitempty" json:"integration,omitempty"`
}
type globalPlugins struct {
	Plugins  map[string]pluginSpec `yaml:"plugins"`
	path     string
	document yaml.Node
}

var pluginIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func enabled(v *bool) bool { return v == nil || *v }
func pluginConfigPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("XDG_CONFIG_HOME must be an absolute path")
	}
	return filepath.Join(dir, "wpdev", "config.yml"), nil
}
func validatePluginSpec(id string, spec pluginSpec) error {
	if !pluginIDPattern.MatchString(id) {
		return fmt.Errorf("invalid plugin ID %q; use lowercase letters, numbers, _ and -", id)
	}
	if spec.Integration != "" && spec.Integration != "remote-media" {
		return fmt.Errorf("plugin %s: unknown integration %q", id, spec.Integration)
	}
	if pluginIDPattern.MatchString(spec.Source) {
		return nil
	}
	u, err := url.Parse(spec.Source)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || !strings.HasSuffix(strings.ToLower(u.Path), ".zip") {
		return fmt.Errorf("plugin %s: source must be a WordPress.org slug or a public HTTPS ZIP URL", id)
	}
	return nil
}
func validatePluginConfig(c *Config) error {
	for _, id := range c.LocalPlugins.Disabled {
		if !pluginIDPattern.MatchString(id) {
			return fmt.Errorf("invalid local_plugins.disabled ID %q", id)
		}
	}
	urls := []string{c.RemoteMedia.URL}
	for id, u := range c.RemoteMedia.Sites {
		if id < 1 {
			return fmt.Errorf("remote_media.sites keys must be positive site IDs")
		}
		urls = append(urls, u)
	}
	for _, u := range urls {
		if u != "" {
			if _, err := absoluteSiteURL(u); err != nil {
				return fmt.Errorf("remote_media: %w", err)
			}
		}
	}
	return nil
}
func loadGlobalPlugins() (*globalPlugins, error) {
	p, err := pluginConfigPath()
	if err != nil {
		return nil, err
	}
	g := &globalPlugins{Plugins: map[string]pluginSpec{}, path: p}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	if err = yaml.Unmarshal(b, &g.document); err != nil {
		return nil, fmt.Errorf("global config %s: %w", p, err)
	}
	if len(g.document.Content) != 1 || g.document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("global config %s must contain a YAML mapping", p)
	}
	if err = g.document.Decode(g); err != nil {
		return nil, err
	}
	if g.Plugins == nil {
		g.Plugins = map[string]pluginSpec{}
	}
	for id, spec := range g.Plugins {
		if spec.Source == "" {
			spec.Source = id
			g.Plugins[id] = spec
		}
		if err = validatePluginSpec(id, spec); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return g, nil
}
func (g *globalPlugins) save() error {
	var plugins yaml.Node
	if err := plugins.Encode(g.Plugins); err != nil {
		return err
	}
	if len(g.document.Content) == 0 {
		g.document = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := g.document.Content[0]
	found := false
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "plugins" {
			// Replace this map so removed entries disappear; retain other global settings.
			plugins.HeadComment, plugins.LineComment, plugins.FootComment = root.Content[i+1].HeadComment, root.Content[i+1].LineComment, root.Content[i+1].FootComment
			root.Content[i+1] = &plugins
			found = true
			break
		}
	}
	if !found {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "plugins"}, &plugins)
	}
	var out bytes.Buffer
	e := yaml.NewEncoder(&out)
	e.SetIndent(2)
	if err := e.Encode(&g.document); err != nil {
		return err
	}
	if err := e.Close(); err != nil {
		return err
	}
	return atomicWrite(g.path, out.Bytes(), 0o600)
}
func desiredPlugins(c *Config, g *globalPlugins) map[string]pluginSpec {
	out := map[string]pluginSpec{}
	if !enabled(c.LocalPlugins.Enabled) {
		return out
	}
	for id, spec := range g.Plugins {
		out[id] = spec
	}
	for _, id := range c.LocalPlugins.Disabled {
		delete(out, id)
	}
	return out
}
func sortedPluginIDs[T any](items map[string]T) []string {
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

var pluginsCmd = &cobra.Command{Use: "plugins", Short: "Manage global local-development plugins"}

func init() {
	var source, integration string
	add := &cobra.Command{Use: "add <id>", Args: cobra.ExactArgs(1), Short: "Add a plugin to your global list (no installation yet)", RunE: func(cmd *cobra.Command, args []string) error {
		g, err := loadGlobalPlugins()
		if err != nil {
			return err
		}
		spec := pluginSpec{Source: source, Integration: integration}
		if spec.Source == "" {
			spec.Source = args[0]
		}
		if err = validatePluginSpec(args[0], spec); err != nil {
			return err
		}
		g.Plugins[args[0]] = spec
		if err = g.save(); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Plugin saved to", g.path, "— run wpdev plugins apply in a project.")
		return nil
	}}
	add.Flags().StringVar(&source, "source", "", "WordPress.org slug or public HTTPS ZIP URL (default: plugin ID)")
	add.Flags().StringVar(&integration, "integration", "", "optional integration: remote-media")
	remove := &cobra.Command{Use: "remove <id>", Args: cobra.ExactArgs(1), Short: "Remove a plugin from your global list", RunE: func(cmd *cobra.Command, args []string) error {
		g, err := loadGlobalPlugins()
		if err != nil {
			return err
		}
		delete(g.Plugins, args[0])
		if err = g.save(); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Global entry removed. Managed copies deactivate on the next apply/start; cached files are retained.")
		return nil
	}}
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "Show your global plugin list and its configuration path", RunE: func(cmd *cobra.Command, args []string) error {
		g, err := loadGlobalPlugins()
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), g.path)
		for _, id := range sortedPluginIDs(g.Plugins) {
			spec := g.Plugins[id]
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", id, spec.Source, spec.Integration)
		}
		return nil
	}}
	var dryRun bool
	apply := &cobra.Command{Use: "apply", Args: cobra.NoArgs, Short: "Install and activate the effective list in this local project", RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		return applyPlugins(c, pluginApplyOptions{DryRun: dryRun}, cmd.OutOrStdout())
	}}
	apply.Flags().BoolVar(&dryRun, "dry-run", false, "show planned changes without downloads or activation")
	update := &cobra.Command{Use: "update [id]", Args: cobra.MaximumNArgs(1), Short: "Explicitly update wpdev-managed plugins in this project", RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		o := pluginApplyOptions{Update: true}
		if len(args) > 0 {
			o.ID = args[0]
		}
		return applyPlugins(c, o, cmd.OutOrStdout())
	}}
	pluginsCmd.AddCommand(add, remove, list, apply, update)
}
