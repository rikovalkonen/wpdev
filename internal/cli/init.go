package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type prompter struct {
	reader   *bufio.Reader
	out      io.Writer
	defaults bool
	err      error
}

func (p *prompter) ask(label, def string) string {
	if p.err != nil {
		return def
	}
	if p.defaults {
		return def
	}
	fmt.Fprintf(p.out, "%s [%s]: ", label, def)
	line, err := p.reader.ReadString('\n')
	if err != nil && !(err == io.EOF && len(line) > 0) {
		p.err = fmt.Errorf("read %s: %w (use --defaults for noninteractive init)", label, err)
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}
func yes(s string) bool { return strings.EqualFold(s, "y") || strings.EqualFold(s, "yes") }

var initDefaults bool
var initCmd = &cobra.Command{Use: "init", Short: "Initialize a project interactively", RunE: func(cmd *cobra.Command, args []string) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	root := filepath.Dir(path)
	p := &prompter{reader: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout(), defaults: initDefaults}
	var c *Config
	if _, err = os.Stat(path); err == nil {
		if initDefaults || !yes(p.ask("Config exists. Overwrite? (y/n)", "n")) {
			if p.err != nil {
				return p.err
			}
			c, err = loadConfig(path)
			if err != nil {
				return err
			}
			fmt.Fprintln(p.out, "Using existing configuration.")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if c == nil {
		c = defaultConfig()
		c.Root = root
		c.ConfigPath = path
		c.Name = p.ask("Project name", filepath.Base(root))
		c.Domain = p.ask("Domain", c.Name+".test")
		c.Web.PHP = p.ask("PHP version", "8.5")
		c.Web.Server = p.ask("Web server (apache/nginx)", "apache")
		c.Web.Docroot = p.ask("Document root", "wp")
		c.Database.Engine = p.ask("Database engine (mariadb/mysql)", "mariadb")
		dbVersion := "11.4"
		if c.Database.Engine == "mysql" {
			dbVersion = "8.4"
		}
		c.Database.Version = p.ask("Database version", dbVersion)
		c.Database.Persist = p.ask("DB persistence (bind/volume)", "bind")
		if c.Database.Persist == "bind" {
			c.Database.DataPath = p.ask("DB data directory", "database")
		}
		c.TLS.Enabled = yes(p.ask("Enable TLS (mkcert)? (y/n)", "y"))
		c.Services.Redis = yes(p.ask("Enable Redis? (y/n)", "y"))
		c.Services.Mailpit = yes(p.ask("Enable Mailpit? (y/n)", "y"))
		c.Services.Adminer = yes(p.ask("Enable Adminer? (y/n)", "y"))
		mode := p.ask("WordPress type (single/subdirectory/subdomain)", "single")
		if mode != "single" {
			c.Multisite.Enabled = true
			c.Multisite.Mode = mode
			c.Multisite.BasePath = p.ask("Network URL base path", "/")
		}
		c.Perf.Excludes = []string{"node_modules", "vendor", ".git"}
		if p.err != nil {
			return p.err
		}
		if err = c.validate(); err != nil {
			return err
		}
		if err = backupFile(c, path); err != nil {
			return err
		}
		if err = saveConfig(path, c); err != nil {
			return err
		}
	}
	if c.Database.Persist == "bind" {
		if err = os.MkdirAll(c.file(c.Database.DataPath), 0o755); err != nil {
			return err
		}
	}
	if err = os.MkdirAll(c.file(".wpdev", "templates"), 0o755); err != nil {
		return err
	}
	if err = os.MkdirAll(c.file(c.Web.Docroot), 0o755); err != nil {
		return err
	}
	if _, err = os.Stat(c.file(c.Web.Docroot, "wp-load.php")); os.IsNotExist(err) {
		if err = writeIfMissing(c.file(c.Web.Docroot, "index.php"), []byte("<?php echo 'wpdev is ready. Install WordPress to get started.';\n")); err != nil {
			return err
		}
	}
	if err = projectIgnore(c); err != nil {
		return err
	}
	if err = ensureTLS(c, true); err != nil {
		return err
	}
	fmt.Fprintln(p.out, "Init complete. Run wpdev start. Use wpdev wp for WordPress commands.")
	return nil
}}

func init() {
	initCmd.Flags().BoolVar(&initDefaults, "defaults", false, "use defaults without prompting; keep existing config")
}
func projectIgnore(c *Config) error {
	p := c.file(".gitignore")
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	marker := "# wpdev local artifacts"
	if strings.Contains(string(b), marker) {
		return nil
	}
	text := "\n" + marker + "\n/.wpdev/certs/\n/.wpdev/backups/\n/.wpdev/db/\n/.wpdev/generated/\n/.wpdev/template-state.json\n/.wpdev/sites.json\n/.wpdev-compose-*.yml\n/docker-compose.yml\n"
	return atomicWrite(p, append(b, []byte(text)...), 0o644)
}
