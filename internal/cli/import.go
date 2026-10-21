package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func addImportFlags(cmd *cobra.Command, o *importOptions) {
	cmd.Flags().BoolVar(&o.DryRun, "dry-run", false, "Validate and preview in an isolated database without publishing")
	cmd.Flags().BoolVarP(&o.Yes, "yes", "y", false, "Confirm replacement of the local database")
	cmd.Flags().StringVar(&o.Map, "map", "", "YAML map from site IDs to local URLs")
	cmd.Flags().StringVar(&o.Prefix, "table-prefix", "", "Select the WordPress table prefix when ambiguous")
	cmd.Flags().StringVar(&o.Mode, "multisite-mode", "", "Source network type when it cannot be detected: subdomain or subdirectory")
}

func localizeDatabase(cmd *cobra.Command, c *Config, file string, opts importOptions) error {
	if opts.Prefix != "" && !sqlIdentifier.MatchString(opts.Prefix) {
		return fmt.Errorf("invalid --table-prefix")
	}
	if opts.Mode != "" && opts.Mode != "subdomain" && opts.Mode != "subdirectory" {
		return fmt.Errorf("invalid --multisite-mode")
	}
	if opts.Files == nil {
		if err := requireWordPress(c); err != nil {
			return err
		}
	}
	if file != "" {
		info, e := os.Stat(file)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("SQL input must be a nonempty regular file")
		}
	}
	source, mode, err := configSource(c)
	originalConfig := append([]byte(nil), source...)
	configMissing := os.IsNotExist(err)
	stageRoot := ""
	if opts.Files != nil {
		stageRoot = opts.Files.dir
		opts.Files.newConfig = configMissing
		if configMissing {
			source, err = os.ReadFile(filepath.Join(stageRoot, "wp-config.php"))
			mode = 0o644
		}
	}
	if err != nil {
		return err
	}
	originalYAML, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		return err
	}
	transformed, err := transformConfig(c, source, localDBValues())
	if err != nil {
		return err
	}
	stage, err := newImportStage(c, []byte(transformed.Source), stageRoot)
	if err != nil {
		return err
	}
	defer stage.close()
	if file == "" {
		file = filepath.Join(stage.dir, "source.sql")
		f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		dump, _ := dbTools(c)
		err = compose(c, nil, f, "exec", "-T", "db", dump, "-uroot", "-proot", "--single-transaction", "wordpress")
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			return fmt.Errorf("read local database (the db service must be running): %w", err)
		}
	}
	if err = stage.importSQL(file); err != nil {
		return fmt.Errorf("temporary database import failed; local database was not replaced: %w", err)
	}
	meta, err := stage.inspect(opts, transformed.Hints.Mode)
	if err != nil {
		return err
	}
	plan, err := makeImportPlan(c, meta, opts)
	if err != nil {
		return err
	}
	plan.Files = opts.Files
	media, err := stage.sourceMedia(source, meta)
	if err != nil {
		return err
	}
	plan.Media, err = planMediaMap(c, plan, media)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Local URL changes:")
	for i, site := range plan.Source.Sites {
		fmt.Fprintf(cmd.OutOrStdout(), "  site %d: %s -> %s\n", site.ID, site.Home, plan.Target.Sites[i].Home)
	}
	localConfig, err := stage.localize(plan, source)
	if err != nil {
		return fmt.Errorf("staged localization failed; active project unchanged: %w", err)
	}
	if opts.DryRun {
		if err = previewPlugins(&plan.Config, cmd.OutOrStdout()); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Preview passed. Active database and project files unchanged.")
		return nil
	}
	normalized := filepath.Join(stage.dir, "localized.sql")
	if err = stage.export(normalized); err != nil {
		return err
	}
	if !opts.Yes {
		if opts.Files != nil {
			fmt.Fprint(cmd.OutOrStdout(), "Replace the local database, apply the listed file changes and update local configuration? [y/N] ")
		} else {
			fmt.Fprint(cmd.OutOrStdout(), "Replace the local database and update local configuration? [y/N] ")
		}
		answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			return fmt.Errorf("import cancelled; active database unchanged (use --yes for automation)")
		}
	}
	// Staging can take time; do not overwrite edits made while it was running.
	current, _, err := configSource(c)
	if configMissing != os.IsNotExist(err) {
		return fmt.Errorf("wp-config.php appeared or disappeared during import; rerun with the new settings")
	}
	if err != nil && !(configMissing && os.IsNotExist(err)) {
		return err
	}
	currentYAML, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, originalConfig) || !bytes.Equal(currentYAML, originalYAML) {
		return fmt.Errorf("configuration changed during import; rerun with the new settings")
	}
	if err = publishImport(c, plan, normalized, localConfig, mode); err != nil {
		return err
	}
	if opts.Files != nil {
		fmt.Fprintln(cmd.OutOrStdout(), "WordPress files and database imported successfully:", plan.Config.URL())
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "Database localized successfully:", plan.Config.URL())
	}
	if err = applyPlugins(c, pluginApplyOptions{}, cmd.OutOrStdout()); err != nil {
		return &postImportPluginError{err}
	}
	return nil
}

type postImportPluginError struct{ err error }

func (e *postImportPluginError) Error() string {
	return "database was localized successfully, but local plugins failed; retry with wpdev plugins apply: " + e.err.Error()
}
func (e *postImportPluginError) Unwrap() error { return e.err }

type snapshotEntry struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}
type importSnapshot struct {
	dir     string
	root    string
	paths   []string
	entries []snapshotEntry
}

func snapshotImport(c *Config) (*importSnapshot, error) {
	return snapshotManaged(c, "import")
}
func snapshotManaged(c *Config, label string, extraPaths ...string) (*importSnapshot, error) {
	s := &importSnapshot{root: c.Root, dir: c.file(".wpdev", "backups", label+"-"+time.Now().UTC().Format("20060102T150405.000000000"))}
	s.paths = []string{filepath.Base(c.ConfigPath), "docker-compose.yml", ".gitignore",
		filepath.Join(c.Web.Docroot, "wp-config.php"), filepath.Join(c.Web.Docroot, ".htaccess"),
		filepath.Join(c.Web.Docroot, "wp-content", "mu-plugins", "wpdev-local.php"),
		".wpdev/generated", ".wpdev/templates", ".wpdev/certs", ".wpdev/sites.json", ".wpdev/template-state.json", ".wpdev/remote-media.json", ".wpdev/plugins-state.json"}
	s.paths = append(s.paths, extraPaths...)
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	for _, rel := range s.paths {
		err := filepath.WalkDir(c.file(rel), func(path string, d os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("managed path is a symlink: %s", path)
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("managed path is not a regular file: %s", path)
			}
			relative, err := filepath.Rel(c.Root, path)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err = atomicWrite(filepath.Join(s.dir, "files", relative), b, 0o600); err != nil {
				return err
			}
			s.entries = append(s.entries, snapshotEntry{relative, uint32(info.Mode().Perm())})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	b, err := json.MarshalIndent(map[string]any{"root": s.root, "paths": s.paths, "files": s.entries}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err = atomicWrite(filepath.Join(s.dir, "manifest.json"), b, 0o600); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *importSnapshot) restore() error {
	for _, rel := range s.paths {
		if err := os.RemoveAll(filepath.Join(s.root, rel)); err != nil {
			return err
		}
	}
	for _, entry := range s.entries {
		b, err := os.ReadFile(filepath.Join(s.dir, "files", entry.Path))
		if err != nil {
			return err
		}
		if err = atomicWrite(filepath.Join(s.root, entry.Path), b, os.FileMode(entry.Mode)); err != nil {
			return err
		}
	}
	return nil
}

func replaceLocalDB(c *Config, path string) error {
	_, client := dbTools(c)
	if err := compose(c, nil, io.Discard, "exec", "-T", "db", client, "-uroot", "-proot", "-e", "DROP DATABASE IF EXISTS wordpress; CREATE DATABASE wordpress CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"); err != nil {
		return err
	}
	f, err := openSQL(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return compose(c, f, os.Stdout, "exec", "-T", "db", client, "--binary-mode=1", "--local-infile=0", "-uroot", "-proot", "wordpress")
}

func publishImport(c *Config, p *importPlan, dump string, config []byte, mode os.FileMode) (err error) {
	if err = os.MkdirAll(c.file(".wpdev"), 0o755); err != nil {
		return err
	}
	lock := c.file(".wpdev", "import.lock")
	if err = os.Mkdir(lock, 0o700); err != nil {
		return fmt.Errorf("cannot acquire import lock %s; another import may be running (remove a stale lock only after checking): %w", lock, err)
	}
	defer os.Remove(lock)
	var filePaths []string
	if p.Files != nil {
		if err = p.Files.check(c); err != nil {
			return err
		}
		filePaths = p.Files.snapshotPaths(c)
	}
	snapshot, err := snapshotManaged(c, "import", filePaths...)
	if err != nil {
		return err
	}
	fmt.Println("Import recovery files:", snapshot.dir)
	var running []string
	_, statErr := os.Stat(c.file("docker-compose.yml"))
	hadCompose := statErr == nil
	if hadCompose {
		out, e := captureCompose(c, "ps", "--services", "--status", "running")
		if e != nil {
			return e
		}
		running = strings.Fields(out)
	}
	dbBackup := ""
	dbChanged := false
	touched := false
	target := &p.Config
	// A failure after publication starts restores both DB and configuration.
	defer func() {
		if err == nil || !touched {
			return
		}
		originalErr := err
		var failures []string
		_, composeErr := os.Stat(target.file("docker-compose.yml"))
		if composeErr == nil {
			if e := compose(target, nil, io.Discard, "stop"); e != nil {
				failures = append(failures, "stop: "+e.Error())
			}
		}
		if dbChanged {
			if e := compose(target, nil, io.Discard, "up", "-d", "--wait", "--wait-timeout", "180", "db"); e != nil {
				failures = append(failures, "database start: "+e.Error())
			} else if e = replaceLocalDB(target, dbBackup); e != nil {
				failures = append(failures, "database restore: "+e.Error())
			}
		}
		// Remove new containers before restoring the old Compose model.
		if composeErr == nil {
			if e := compose(target, nil, io.Discard, "down", "--remove-orphans"); e != nil {
				failures = append(failures, "container cleanup: "+e.Error())
			}
		}
		if e := snapshot.restore(); e != nil {
			failures = append(failures, "file restore: "+e.Error())
		} else if len(failures) == 0 && hadCompose && len(running) > 0 {
			if e = compose(c, nil, os.Stdout, append([]string{"up", "-d", "--build", "--wait", "--wait-timeout", "180"}, running...)...); e != nil {
				failures = append(failures, "restart: "+e.Error())
			}
		}
		if len(failures) > 0 {
			err = fmt.Errorf("%w; recovery incomplete: %s. Database backup: %s; configuration files and manifest: %s. Restore the SQL into the local db service and copy the manifest's files back before restarting", originalErr, strings.Join(failures, "; "), dbBackup, snapshot.dir)
		} else {
			err = fmt.Errorf("%w; previous database and configuration restored (backup: %s)", originalErr, snapshot.dir)
		}
	}()
	touched = true
	if hadCompose {
		var stop []string
		for _, service := range running {
			if service != "db" {
				stop = append(stop, service)
			}
		}
		if len(stop) > 0 {
			if err = compose(c, nil, os.Stdout, append([]string{"stop"}, stop...)...); err != nil {
				return err
			}
		}
	}
	hosts := []string{target.Domain}
	for _, site := range p.Target.Sites {
		hosts = append(hosts, site.Domain)
	}
	b, _ := json.Marshal(hosts)
	if err = atomicWrite(target.file(".wpdev", "sites.json"), b, 0o644); err != nil {
		return err
	}
	if err = ensureTLS(target, false); err != nil {
		return err
	}
	if err = renderTemplates(target); err != nil {
		return err
	}
	if err = validateLocalRuntime(target); err != nil {
		return err
	}
	if err = compose(target, nil, os.Stdout, "up", "-d", "--wait", "--wait-timeout", "180", "db"); err != nil {
		return err
	}
	dbBackup, err = dumpDatabase(target)
	if err != nil {
		return err
	}
	// Publish a concrete recovery instruction alongside the configuration.
	_, client := dbTools(target)
	resetArgs := append([]string{"docker"}, composeArgs(target, "exec", "-T", "db", client, "-uroot", "-proot", "-e", "DROP DATABASE IF EXISTS wordpress; CREATE DATABASE wordpress;")...)
	for i := range resetArgs {
		resetArgs[i] = shellQuote(resetArgs[i])
	}
	recovery := fmt.Sprintf("Database backup: %s\nOriginal files: files/ (paths and permissions in manifest.json)\nKeep PHP and HTTP services stopped while recovering. Start only the db service.\nClear the incomplete local database:\n%s\nRestore SQL:\nwpdev --config %s db import %s\nRestore files from this snapshot before starting the previous project. Remove new managed files not listed in manifest.json and restore recorded permissions.\n", dbBackup, strings.Join(resetArgs, " "), shellQuote(c.ConfigPath), shellQuote(dbBackup))
	if err = atomicWrite(filepath.Join(snapshot.dir, "RECOVERY.txt"), []byte(recovery), 0o600); err != nil {
		return err
	}
	dbChanged = true
	if err = replaceLocalDB(target, dump); err != nil {
		return err
	}
	if p.Files != nil {
		if err = p.Files.publish(target); err != nil {
			return err
		}
	}
	if err = atomicWrite(target.file(target.Web.Docroot, "wp-config.php"), config, mode); err != nil {
		return err
	}
	if err = saveConfig(target.ConfigPath, target); err != nil {
		return err
	}
	if err = writeMediaMap(target, p.Media); err != nil {
		return err
	}
	if target.Web.Server == "apache" {
		var rules []byte
		rules, err = plannedHTAccess(target)
		if err != nil {
			return err
		}
		if err = atomicWrite(target.file(target.Web.Docroot, ".htaccess"), rules, 0o644); err != nil {
			return err
		}
	}
	if err = startProject(target, false); err != nil {
		return err
	}
	if err = verifyImportedSites(target, p.Target.Sites); err != nil {
		return err
	}
	if err = projectIgnore(target); err != nil {
		return err
	}
	*c = *target
	return nil
}

func validateLocalRuntime(c *Config) error {
	raw, err := captureCompose(c, "config", "--format", "json")
	if err != nil {
		return err
	}
	var model struct {
		Services map[string]struct {
			Environment map[string]string `json:"environment"`
		} `json:"services"`
	}
	if err = json.Unmarshal([]byte(raw), &model); err != nil {
		return err
	}
	env := model.Services["php"].Environment
	if env["WPDEV_LOCAL"] != "1" {
		return fmt.Errorf("the php service must set WPDEV_LOCAL=1; update your custom Compose template before importing")
	}
	for key, expected := range localDBValues() {
		if value, exists := env["WPDEV_"+key]; exists && value != expected {
			return fmt.Errorf("php environment WPDEV_%s overrides the local database; remove it before importing", key)
		}
	}
	return nil
}

func verifyImportedSites(c *Config, sites []importSite) error {
	// Validate real WP bootstrap and HTTP routing. Never follow a redirect to a
	// production hostname while checking a newly imported site's home page.
	hosts := map[string]bool{}
	for _, s := range sites {
		hosts[s.Domain] = true
	}
	payload, _ := json.Marshal(map[string]any{"sites": sites, "hosts": hosts})
	code := `$r=json_decode(stream_get_contents(STDIN),true);foreach($r['sites'] as $s){$url=$s['home'];for($i=0;$i<6;$i++){$ch=curl_init($url);curl_setopt_array($ch,[CURLOPT_RETURNTRANSFER=>true,CURLOPT_HEADER=>true,CURLOPT_TIMEOUT=>20]);$body=curl_exec($ch);if($body===false){fwrite(STDERR,curl_error($ch));exit(1);}$status=curl_getinfo($ch,CURLINFO_RESPONSE_CODE);$next=curl_getinfo($ch,CURLINFO_REDIRECT_URL);if($status>=400||$status<200){fwrite(STDERR,"HTTP $status for $url\n");exit(1);}if(!$next)break;if(!isset($r['hosts'][parse_url($next,PHP_URL_HOST)])){fwrite(STDERR,"External redirect: $next\n");exit(1);}$url=$next;}if($i===6){fwrite(STDERR,"Redirect loop\n");exit(1);}}`
	for _, s := range sites {
		out, err := captureWP(c, "--url="+s.Home, "option", "get", "home")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != s.Home {
			return fmt.Errorf("site %d still resolves to %s", s.ID, strings.TrimSpace(out))
		}
	}
	return compose(c, bytes.NewReader(payload), os.Stdout, "exec", "-T", "php", "php", "-r", code)
}
