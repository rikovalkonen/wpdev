package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func wpArgs(c *Config, args ...string) []string {
	// Run as the host owner when supported so WP-CLI never creates root-owned
	// files in the bind mount. HOME/cache remain writable for arbitrary UIDs.
	a := []string{"exec", "-T"}
	if os.Getuid() >= 0 {
		a = append(a, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	}
	a = append(a, "-e", "HOME=/tmp", "-e", "WP_CLI_CACHE_DIR=/tmp/wpdev-wp-cli-cache", "php", "php", "-d", "error_reporting=E_ALL & ~E_DEPRECATED", "/usr/local/bin/wp", "--allow-root", "--path="+c.containerDocroot())
	urlSupplied := false
	for _, arg := range args {
		if arg == "--url" || strings.HasPrefix(arg, "--url=") {
			urlSupplied = true
			break
		}
	}
	if !urlSupplied {
		a = append(a, "--url="+c.URL())
	}
	return append(a, args...)
}
func runWP(c *Config, in io.Reader, out io.Writer, args ...string) error {
	return compose(c, in, out, wpArgs(c, args...)...)
}
func captureWP(c *Config, args ...string) (string, error) {
	var b bytes.Buffer
	err := runWP(c, nil, &b, args...)
	return b.String(), err
}

var wpCmd = &cobra.Command{Use: "wp <arguments...>", Short: "Run WP-CLI in the PHP container (put --config before wp)", DisableFlagParsing: true, Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	c, err := projectConfig()
	if err != nil {
		return err
	}
	return runWP(c, cmd.InOrStdin(), cmd.OutOrStdout(), args...)
}}

type wpNetworkState struct {
	Installed bool   `json:"installed"`
	Multisite bool   `json:"multisite"`
	Subdomain bool   `json:"subdomain"`
	Domain    string `json:"domain"`
	BasePath  string `json:"base_path"`
}

func networkState(c *Config) (wpNetworkState, error) {
	var state wpNetworkState
	if _, err := os.Stat(c.file(c.Web.Docroot, "wp-load.php")); os.IsNotExist(err) {
		return state, nil
	} else if err != nil {
		return state, err
	}
	if _, err := os.Stat(c.file(c.Web.Docroot, "wp-config.php")); os.IsNotExist(err) {
		return state, nil
	} else if err != nil {
		return state, err
	}
	probe := exec.Command("docker", composeArgs(c, wpArgs(c, "core", "is-installed")...)...)
	probe.Dir = c.Root
	var stdout, stderr bytes.Buffer
	probe.Stdout = &stdout
	probe.Stderr = &stderr
	if err := executeProcess(probe); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && stdout.Len() == 0 && stderr.Len() == 0 {
			return state, nil
		}
		return state, fmt.Errorf("check WordPress installation: %w\n%s%s", err, stdout.String(), stderr.String())
	}
	out, err := captureWP(c, "eval", `echo json_encode(['installed'=>true,'multisite'=>is_multisite(),'subdomain'=>is_multisite() && is_subdomain_install(),'domain'=>defined('DOMAIN_CURRENT_SITE') ? DOMAIN_CURRENT_SITE : parse_url(home_url(), PHP_URL_HOST),'base_path'=>defined('PATH_CURRENT_SITE') ? PATH_CURRENT_SITE : (parse_url(home_url('/'), PHP_URL_PATH) ?: '/')]);`)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal([]byte(out), &state)
	return state, err
}
func checkNetwork(c *Config, s wpNetworkState) error {
	if !c.Multisite.Enabled {
		return fmt.Errorf("enable multisite in the project configuration first")
	}
	if !s.Installed || !s.Multisite {
		return fmt.Errorf("WordPress multisite is not installed; use wpdev multisite install or convert")
	}
	if s.Subdomain != (c.Multisite.Mode == "subdomain") || s.Domain != c.Domain || s.BasePath != c.Multisite.BasePath {
		return fmt.Errorf("WordPress network (domain=%s, base=%s, subdomain=%t) does not match .wpdev configuration; changing YAML does not migrate a network", s.Domain, s.BasePath, s.Subdomain)
	}
	return nil
}
func validateSiteHost(c *Config, h string) error {
	if h == c.Domain {
		return nil
	}
	label := strings.TrimSuffix(h, "."+c.Domain)
	if !strings.HasSuffix(h, "."+c.Domain) || !hostPattern.MatchString(label) || len(label) > 63 || len(h) > 253 {
		return fmt.Errorf("site domain %q is outside this project's one-level subdomains; domain migration is not automatic", h)
	}
	if label == "mail" && c.Services.Mailpit || label == "db" && c.Services.Adminer {
		return fmt.Errorf("site domain %q conflicts with an enabled service", h)
	}
	return nil
}
func syncMultisite(c *Config, apply bool) error {
	s, err := networkState(c)
	if err != nil {
		return err
	}
	if err = checkNetwork(c, s); err != nil {
		return err
	}
	raw, err := captureWP(c, "site", "list", "--fields=domain,path", "--format=json")
	if err != nil {
		return err
	}
	var sites []struct {
		Domain string `json:"domain"`
		Path   string `json:"path"`
	}
	if err = json.Unmarshal([]byte(raw), &sites); err != nil {
		return err
	}
	hosts := []string{c.Domain}
	for _, site := range sites {
		h := strings.ToLower(site.Domain)
		if c.Multisite.Mode == "subdirectory" && h != c.Domain {
			return fmt.Errorf("site domain %q requires an explicit domain migration", h)
		}
		if err = validateSiteHost(c, h); err != nil {
			return err
		}
		if !strings.HasPrefix(site.Path, c.Multisite.BasePath) {
			return fmt.Errorf("site path %q is outside network base %s", site.Path, c.Multisite.BasePath)
		}
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	hosts = uniqueStrings(hosts)
	b, err := json.MarshalIndent(hosts, "", "  ")
	if err != nil {
		return err
	}
	p := c.file(".wpdev", "sites.json")
	old, readErr := os.ReadFile(p)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if !bytes.Equal(b, old) {
		if err = atomicWrite(p, b, 0o644); err != nil {
			return err
		}
	}
	if apply {
		if err = tlsFingerprint(c); err != nil {
			return err
		}
		if err = renderTemplates(c); err != nil {
			return err
		}
		// Recreate Caddy aliases and the MU-plugin mount together, keeping the DB.
		if err = compose(c, nil, os.Stdout, "up", "-d", "--wait", "--wait-timeout", "180"); err != nil {
			return err
		}
	}
	fmt.Println("Synchronized multisite domains:", strings.Join(hosts, ", "))
	return nil
}

var multisiteCmd = &cobra.Command{Use: "multisite", Short: "Create, inspect and synchronize WordPress networks"}
var siteCmd = &cobra.Command{Use: "site", Short: "Manage sites in the local WordPress network"}

func requireWordPress(c *Config) error {
	for _, name := range []string{"wp-load.php", "wp-config.php"} {
		if _, err := os.Stat(c.file(c.Web.Docroot, name)); err != nil {
			return fmt.Errorf("prepare %s first (wpdev wp core download / config create): %w", name, err)
		}
	}
	return nil
}
func installNetworkCommand(convert bool) *cobra.Command {
	use := "install"
	if convert {
		use = "convert"
	}
	var title, user, email, password string
	cmd := &cobra.Command{Use: use, Args: cobra.NoArgs, Short: map[bool]string{true: "Convert single-site WordPress after a backup", false: "Install a new network using prepared WordPress files"}[convert], RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		if !c.Multisite.Enabled {
			return fmt.Errorf("set multisite.enabled and multisite.mode in the config first")
		}
		if err = requireWordPress(c); err != nil {
			return err
		}
		state, err := networkState(c)
		if err != nil {
			return err
		}
		if state.Multisite {
			if err = checkNetwork(c, state); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Network already installed; synchronizing.")
			return syncMultisite(c, true)
		}
		if convert && !state.Installed {
			return fmt.Errorf("no single-site installation exists; use multisite install")
		}
		if !convert && state.Installed {
			return fmt.Errorf("WordPress is already installed; use multisite convert")
		}
		if convert && (!strings.EqualFold(state.Domain, c.Domain) || state.BasePath != c.Multisite.BasePath) {
			return fmt.Errorf("single-site URL does not match the requested local network; migrate its domain/base explicitly before conversion")
		}
		p := &prompter{reader: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout()}
		if title == "" {
			title = p.ask("Network title", c.Name)
		}
		if !convert {
			if user == "" {
				user = p.ask("Admin username", "admin")
			}
			if email == "" {
				email = p.ask("Admin email", "")
			}
			if email == "" {
				return fmt.Errorf("admin email is required")
			}
		}
		if p.err != nil {
			return p.err
		}
		// Validate the rewrite edit before making database changes.
		ht, err := plannedHTAccess(c)
		if err != nil {
			return err
		}
		if err = backupFile(c, c.file(c.Web.Docroot, "wp-config.php")); err != nil {
			return err
		}
		if c.Web.Server == "apache" {
			if err = backupFile(c, c.file(c.Web.Docroot, ".htaccess")); err != nil {
				return err
			}
		}
		if convert {
			backup, err := dumpDatabase(c)
			if err != nil {
				return fmt.Errorf("conversion aborted because the database backup failed: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Database backup:", backup)
		}
		a := []string{"core", "multisite-" + use, "--title=" + title, "--base=" + c.Multisite.BasePath, "--url=" + c.URL()}
		if c.Multisite.Mode == "subdomain" {
			a = append(a, "--subdomains")
		}
		if !convert {
			a = append(a, "--admin_user="+user, "--admin_email="+email, "--skip-email")
			if password != "" {
				a = append(a, "--admin_password="+password)
			}
		}
		if err = runWP(c, cmd.InOrStdin(), cmd.OutOrStdout(), a...); err != nil {
			return fmt.Errorf("network operation failed; backups are in .wpdev/backups and .wpdev/db: %w", err)
		}
		if c.Web.Server == "apache" {
			if err = atomicWrite(c.file(c.Web.Docroot, ".htaccess"), ht, 0o644); err != nil {
				return err
			}
		}
		return syncMultisite(c, true)
	}}
	cmd.Flags().StringVar(&title, "title", "", "network title")
	if !convert {
		cmd.Flags().StringVar(&user, "admin-user", "", "administrator username")
		cmd.Flags().StringVar(&email, "admin-email", "", "administrator email")
		cmd.Flags().StringVar(&password, "admin-password", "", "administrator password (random when omitted)")
	}
	return cmd
}
func init() {
	multisiteCmd.AddCommand(installNetworkCommand(false), installNetworkCommand(true), &cobra.Command{Use: "status", Short: "Check the installed network against project settings", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		s, err := networkState(c)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(s, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return checkNetwork(c, s)
	}}, &cobra.Command{Use: "sync", Short: "Refresh site domains, routing and Docker DNS aliases", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		return syncMultisite(c, true)
	}})
	siteCmd.AddCommand(&cobra.Command{Use: "list", Short: "List sites in the local network", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		s, err := networkState(c)
		if err != nil {
			return err
		}
		if err = checkNetwork(c, s); err != nil {
			return err
		}
		return runWP(c, nil, cmd.OutOrStdout(), "site", "list")
	}})
	var title, email string
	create := &cobra.Command{Use: "create <slug>", Args: cobra.ExactArgs(1), Short: "Create a site and synchronize its DNS alias", RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		slug := args[0]
		if !hostPattern.MatchString(slug) || len(slug) > 63 {
			return fmt.Errorf("slug must be a DNS-compatible single label")
		}
		s, err := networkState(c)
		if err != nil {
			return err
		}
		if err = checkNetwork(c, s); err != nil {
			return err
		}
		if c.Multisite.Mode == "subdomain" {
			if err = validateSiteHost(c, slug+"."+c.Domain); err != nil {
				return err
			}
		}
		a := []string{"site", "create", "--slug=" + slug, "--url=" + c.URL()}
		if title != "" {
			a = append(a, "--title="+title)
		}
		if email != "" {
			a = append(a, "--email="+email)
		}
		if err = runWP(c, nil, cmd.OutOrStdout(), a...); err != nil {
			return err
		}
		return syncMultisite(c, true)
	}}
	create.Flags().StringVar(&title, "title", "", "site title")
	create.Flags().StringVar(&email, "email", "", "site administrator email")
	siteCmd.AddCommand(create)
}

func rewriteRules(c *Config) string {
	if !c.Multisite.Enabled {
		return "# BEGIN WordPress\nRewriteEngine On\nRewriteRule .* - [E=HTTP_AUTHORIZATION:%{HTTP:Authorization}]\nRewriteBase /\nRewriteRule ^index\\.php$ - [L]\nRewriteCond %{REQUEST_FILENAME} !-f\nRewriteCond %{REQUEST_FILENAME} !-d\nRewriteRule . /index.php [L]\n# END WordPress\n"
	}
	base := c.Multisite.BasePath
	// Files live at the physical document root, even with a network URL prefix.
	prefix := strings.TrimPrefix(base, "/")
	sub := ""
	if c.Multisite.Mode == "subdirectory" {
		sub = "([_0-9a-zA-Z-]+/)?"
	}
	return "# BEGIN WordPress Multisite\nRewriteEngine On\nRewriteRule .* - [E=HTTP_AUTHORIZATION:%{HTTP:Authorization}]\nRewriteBase /\nRewriteRule ^index\\.php$ - [L]\nRewriteRule ^(" + prefix + sub + "wp-admin)$ $1/ [R=301,L]\nRewriteCond %{REQUEST_FILENAME} -f [OR]\nRewriteCond %{REQUEST_FILENAME} -d\nRewriteRule ^ - [L]\nRewriteRule ^" + prefix + sub + "(wp-(content|admin|includes).*) $" + map[bool]string{true: "2", false: "1"}[sub != ""] + " [L]\nRewriteRule ^" + prefix + sub + "(.*\\.php)$ $" + map[bool]string{true: "2", false: "1"}[sub != ""] + " [L]\nRewriteRule . index.php [L]\n# END WordPress Multisite\n"
}
func plannedHTAccess(c *Config) ([]byte, error) {
	if c.Web.Server != "apache" {
		return nil, nil
	}
	b, err := os.ReadFile(c.file(c.Web.Docroot, ".htaccess"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	text := string(b)
	rules := rewriteRules(c)
	// Replace only the managed WordPress block. Preserve media/env directives.
	re := regexp.MustCompile(`(?ms)^# BEGIN WordPress(?: Multisite)?\r?\n.*?^# END WordPress(?: Multisite)?\r?$`)
	matches := re.FindAllStringIndex(text, -1)
	if len(matches) > 1 {
		return nil, fmt.Errorf("multiple WordPress rewrite blocks in .htaccess; consolidate them before conversion")
	}
	if len(matches) == 1 {
		i := matches[0]
		return []byte(text[:i[0]] + strings.TrimSuffix(rules, "\n") + text[i[1]:]), nil
	}
	if strings.Contains(text, "# BEGIN WordPress") || strings.Contains(text, "# END WordPress") {
		return nil, fmt.Errorf("incomplete WordPress rewrite block in .htaccess")
	}
	return []byte(strings.TrimRight(text, "\n") + "\n\n" + rules), nil
}

const wordpressLoader = `<?php
/** Generated by wpdev; active only inside the local container. */
if (getenv('WPDEV_LOCAL') === '1' && is_readable('/opt/wpdev/wpdev-local.php')) {
    require '/opt/wpdev/wpdev-local.php';
}
`

func ensureWordPressLoader(c *Config) error {
	p := c.file(c.Web.Docroot, "wp-content", "mu-plugins", "wpdev-local.php")
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && string(b) != wordpressLoader {
		return fmt.Errorf("%s contains user code; rename it before enabling wpdev's local integrations", p)
	}
	if err = atomicWrite(p, []byte(wordpressLoader), 0o644); err != nil {
		return err
	}
	ignore := c.file(".gitignore")
	b, err = os.ReadFile(ignore)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	line := "/" + strings.TrimPrefix(c.Web.Docroot+"/wp-content/mu-plugins/wpdev-local.php", "./")
	if !strings.Contains("\n"+string(b)+"\n", "\n"+line+"\n") {
		return atomicWrite(ignore, append(b, []byte("\n"+line+"\n")...), 0o644)
	}
	return nil
}

func phpLiteral(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'") + "'"
}
