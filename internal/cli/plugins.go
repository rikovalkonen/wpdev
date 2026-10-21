package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

type pluginApplyOptions struct {
	DryRun, Update bool
	ID             string
}

func previewPlugins(c *Config, out io.Writer) error {
	g, err := loadGlobalPlugins()
	if err != nil {
		return err
	}
	for _, id := range sortedPluginIDs(desiredPlugins(c, g)) {
		fmt.Fprintf(out, "Local plugin: %s (%s), activate on %s\n", id, g.Plugins[id].Source, map[bool]string{true: "the entire network", false: "this site"}[c.Multisite.Enabled])
	}
	return nil
}
func pluginWP(c *Config, out io.Writer, args ...string) error {
	return runWP(c, nil, out, append([]string{"--skip-plugins", "--skip-themes", "--skip-packages"}, args...)...)
}
func validatePluginDatabase(c *Config) error {
	if err := validateLocalRuntime(c); err != nil {
		return err
	}
	out, err := captureWP(c, "--skip-plugins", "--skip-themes", "--skip-packages", "eval", `echo (DB_HOST === 'db' && DB_NAME === 'wordpress' && DB_USER === 'wp') ? 'local' : 'other';`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "local" {
		return fmt.Errorf("plugin automation requires wpdev's local database settings; run wpdev config local first")
	}
	return nil
}
func pluginActive(p installedPlugin, multi bool) bool {
	if multi {
		return p.Network
	}
	return p.Active
}

func applyPlugins(c *Config, o pluginApplyOptions, out io.Writer) (err error) {
	g, err := loadGlobalPlugins()
	if err != nil {
		return err
	}
	desired := desiredPlugins(c, g)
	for _, spec := range desired {
		if spec.Integration == "remote-media" && enabled(c.RemoteMedia.Enabled) {
			m, e := readMediaMap(c)
			if e != nil {
				return e
			}
			if len(m.Sites) == 0 && c.RemoteMedia.URL == "" && len(c.RemoteMedia.Sites) == 0 {
				fmt.Fprintln(out, "Remote media is pending: import a source database or set remote_media.url/sites. Media URLs remain local until then.")
			}
			break
		}
	}
	old, err := readPluginState(c)
	if err != nil {
		return err
	}
	if o.ID != "" {
		r, ok := old.Plugins[o.ID]
		if !ok || !r.Managed || !r.Enabled {
			return fmt.Errorf("%s is not an enabled wpdev-managed plugin in this project", o.ID)
		}
		if _, ok = desired[o.ID]; !ok {
			return fmt.Errorf("%s is disabled or absent from the global list", o.ID)
		}
	}
	if o.Update {
		selected := map[string]pluginSpec{}
		for id, spec := range desired {
			r := old.Plugins[id]
			if r.Managed && r.Enabled && (o.ID == "" || o.ID == id) {
				selected[id] = spec
			}
		}
		desired = selected
		if len(desired) == 0 {
			fmt.Fprintln(out, "No enabled wpdev-managed plugins to update. Use wpdev plugins apply to install the global list.")
			return nil
		}
	}
	if o.DryRun {
		if err = previewPlugins(c, out); err != nil {
			return err
		}
		for _, id := range sortedPluginIDs(old.Plugins) {
			r := old.Plugins[id]
			if _, ok := desired[id]; !ok && r.Managed && r.Enabled {
				fmt.Fprintln(out, "Deactivate managed plugin:", id)
			}
		}
		fmt.Fprintln(out, "Preview only; no downloads, activation or updates.")
		return nil
	}
	if len(desired) == 0 && len(old.Plugins) == 0 {
		return nil
	}
	if err = os.MkdirAll(c.file(".wpdev"), 0o755); err != nil {
		return err
	}
	// Share the publication lock with database imports: neither operation may
	// back up or replace a database while the other is changing it.
	lock := c.file(".wpdev", "import.lock")
	if err = os.Mkdir(lock, 0o700); err != nil {
		return fmt.Errorf("cannot acquire %s; an import/plugin operation may be running: %w", lock, err)
	}
	defer os.Remove(lock)
	current, err := readPluginState(c)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, old) {
		return fmt.Errorf("plugin state changed while acquiring the project lock; rerun wpdev plugins apply")
	}
	state, err := networkState(c)
	if err != nil {
		return err
	}
	if !state.Installed {
		fmt.Fprintln(out, "Local plugins pending: WordPress is not installed yet. Run wpdev start after installation.")
		return nil
	}
	if err = validatePluginDatabase(c); err != nil {
		return err
	}
	for _, spec := range desired {
		if spec.Integration == "remote-media" {
			if err = checkMediaHTAccess(c); err != nil {
				return err
			}
		}
	}
	inventory, err := pluginInventory(c)
	if err != nil {
		return err
	}
	next := pluginState{Plugins: map[string]pluginRecord{}}
	for id, r := range old.Plugins {
		next.Plugins[id] = r
	}
	var installs, activate, deactivate []string
	for _, id := range sortedPluginIDs(desired) {
		spec := desired[id]
		record, known := old.Plugins[id]
		installed, exists := inventory[id]
		if installed.Ambiguous {
			return fmt.Errorf("plugin %s has multiple entrypoints; exclude it with local_plugins.disabled and manage it with WP-CLI", id)
		}
		cacheExists := false
		if known && record.Managed {
			_, e := os.Stat(c.file(".wpdev", "plugins", record.File))
			cacheExists = e == nil
			if e != nil && !os.IsNotExist(e) {
				return e
			}
		}
		if exists && (!known || !record.Managed) {
			record = pluginRecord{pluginSpec: spec, File: installed.File, Version: installed.Version, Managed: false, Enabled: true}
			fmt.Fprintln(out, "Using project-owned plugin (files and updates preserved):", id)
		} else if !cacheExists || o.Update && (o.ID == "" || o.ID == id) {
			installs = append(installs, id)
		} else {
			record.Enabled = true
			record.Integration = spec.Integration
		}
		if !exists || !pluginActive(installed, state.Multisite) {
			activate = append(activate, id)
		}
		next.Plugins[id] = record
	}
	for _, id := range sortedPluginIDs(old.Plugins) {
		r := old.Plugins[id]
		if _, ok := desired[id]; !o.Update && !ok && r.Enabled {
			if r.Managed {
				deactivate = append(deactivate, id)
			}
			r.Enabled = false
			next.Plugins[id] = r
		}
	}
	if len(installs) == 0 && len(activate) == 0 && len(deactivate) == 0 && reflect.DeepEqual(next, old) {
		if err = loadPluginRuntime(c); err != nil {
			return err
		}
		templates, e := readTemplateState(c)
		if e != nil {
			return e
		}
		source, e := templateSource(c, "remote-media.php.tmpl", templates, true)
		if e != nil {
			return e
		}
		wanted, e := executeTemplate("remote-media.php.tmpl", source, dataFor(c))
		if e != nil {
			return e
		}
		current, e := os.ReadFile(c.file(".wpdev", "generated", "remote-media.php"))
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if bytes.Equal(current, wanted) {
			if err = validatePluginMounts(c); err != nil {
				return err
			}
			fmt.Fprintln(out, "Local plugins are current (no automatic updates).")
			return nil
		}
	}
	snapshot, err := snapshotManaged(c, "plugins", ".wpdev/plugins")
	if err != nil {
		return err
	}
	backup, err := dumpDatabase(c)
	if err != nil {
		return fmt.Errorf("plugin operation aborted because database backup failed: %w", err)
	}
	fmt.Fprintln(out, "Plugin recovery files:", snapshot.dir, "Database:", backup)
	_, client := dbTools(c)
	reset := append([]string{"docker"}, composeArgs(c, "exec", "-T", "db", client, "-uroot", "-proot", "-e", "DROP DATABASE IF EXISTS wordpress; CREATE DATABASE wordpress CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;")...)
	for i := range reset {
		reset[i] = shellQuote(reset[i])
	}
	recovery := "Local plugin operation backup\nDatabase: " + backup + "\nManaged files: files/ (manifest.json records paths and permissions).\nKeep PHP/web/Caddy stopped while recovering; start only db.\nClear the incomplete local database:\n" + strings.Join(reset, " ") + "\nRestore SQL:\nwpdev --config " + shellQuote(c.ConfigPath) + " db import " + shellQuote(backup) + "\nRestore the snapshot's managed paths and permissions, removing new managed files absent from manifest.json. Review/fix the global plugin list before starting wpdev again.\n"
	if err = atomicWrite(filepath.Join(snapshot.dir, "RECOVERY.txt"), []byte(recovery), 0o600); err != nil {
		return err
	}
	touched := true
	defer func() {
		if err == nil || !touched {
			return
		}
		original := err
		var failures []string
		if e := compose(c, nil, io.Discard, "stop"); e != nil {
			failures = append(failures, "stop: "+e.Error())
		}
		if e := compose(c, nil, io.Discard, "up", "-d", "--wait", "--wait-timeout", "180", "db"); e != nil {
			failures = append(failures, "database start: "+e.Error())
		} else if e = replaceLocalDB(c, backup); e != nil {
			failures = append(failures, "database restore: "+e.Error())
		}
		if e := compose(c, nil, io.Discard, "down", "--remove-orphans"); e != nil {
			failures = append(failures, "container cleanup: "+e.Error())
		}
		if e := snapshot.restore(); e != nil {
			failures = append(failures, "file restore: "+e.Error())
		}
		if len(failures) == 0 {
			if e := compose(c, nil, out, "up", "-d", "--build", "--wait", "--wait-timeout", "180"); e != nil {
				failures = append(failures, "restart: "+e.Error())
			}
		}
		if len(failures) > 0 {
			err = fmt.Errorf("%w; plugin recovery incomplete: %s. Database backup: %s; files and instructions: %s", original, strings.Join(failures, "; "), backup, snapshot.dir)
		} else {
			err = fmt.Errorf("%w; previous local plugin/database state restored (backup: %s)", original, snapshot.dir)
		}
	}()
	type staged struct {
		id, dir string
		record  pluginRecord
	}
	var packages []staged
	for _, id := range installs {
		dir, r, cleanup, e := stagePlugin(c, id, desired[id], out)
		defer cleanup()
		if e != nil {
			return fmt.Errorf("stage plugin %s: %w", id, e)
		}
		next.Plugins[id] = r
		previous := old.Plugins[id]
		if previous.File != "" && previous.File != r.File {
			deactivate = append(deactivate, id)
			activate = append(activate, id)
		}
		// An unchanged package must retain its directory inode: Docker bind
		// mounts otherwise keep the removed directory when Compose sees no diff.
		_, cacheErr := os.Stat(c.file(".wpdev", "plugins", r.File))
		if cacheErr != nil && !os.IsNotExist(cacheErr) {
			return cacheErr
		}
		if previous.Digest != r.Digest || previous.File != r.File || os.IsNotExist(cacheErr) {
			packages = append(packages, staged{id, dir, r})
		}
	}
	sort.Strings(activate)
	activate = uniqueStrings(activate)
	sort.Strings(deactivate)
	deactivate = uniqueStrings(deactivate)
	// Deactivate before removing mounts. Project-owned plugins are never retired.
	for _, id := range deactivate {
		if _, ok := inventory[id]; !ok {
			continue
		}
		if state.Multisite {
			if err = pluginWP(c, out, "plugin", "deactivate", id, "--network"); err != nil {
				return err
			}
			urls, e := captureWP(c, "--skip-plugins", "--skip-themes", "site", "list", "--field=url")
			if e != nil {
				return e
			}
			for _, u := range strings.Fields(urls) {
				if err = pluginWP(c, out, "--url="+u, "plugin", "deactivate", id); err != nil {
					return err
				}
			}
		} else if err = pluginWP(c, out, "plugin", "deactivate", id); err != nil {
			return err
		}
	}
	if len(packages) > 0 {
		// Existing plugin directories are bind-mounted: stop readers before
		// replacing them, then recreate services through the new fingerprint.
		services := []string{"stop", "php", "caddy"}
		if c.Web.Server == "nginx" {
			services = append(services, "web")
		}
		if err = compose(c, nil, out, services...); err != nil {
			return err
		}
		// A repaired cache can have the same fingerprint as before. Remove
		// stopped readers so their bind mounts always open the new directories.
		if err = compose(c, nil, out, append([]string{"rm", "-f"}, services[1:]...)...); err != nil {
			return err
		}
	}
	for _, pkg := range packages {
		dest := c.file(".wpdev", "plugins", pkg.id)
		if err = os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err = os.RemoveAll(dest); err != nil {
			return err
		}
		if err = os.Rename(pkg.dir, dest); err != nil {
			return err
		}
	}
	if err = writePluginState(c, next); err != nil {
		return err
	}
	if err = startStack(c, false); err != nil {
		return err
	}
	if err = validatePluginMounts(c); err != nil {
		return err
	}
	for _, id := range activate {
		args := []string{"plugin", "activate", id}
		if state.Multisite {
			args = append(args, "--network")
		}
		if err = pluginWP(c, out, args...); err != nil {
			return fmt.Errorf("activate %s: %w", id, err)
		}
	}
	// A fresh WP process catches fatal errors introduced by an update as well
	// as activation failures, without waiting for the next browser request.
	if _, err = captureWP(c, "--skip-themes", "eval", `echo 'local plugins loaded';`); err != nil {
		return fmt.Errorf("plugin verification: %w", err)
	}
	verified, err := pluginInventory(c)
	if err != nil {
		return err
	}
	for id := range desired {
		if !pluginActive(verified[id], state.Multisite) {
			return fmt.Errorf("plugin %s did not activate", id)
		}
	}
	if err = projectIgnore(c); err != nil {
		return err
	}
	touched = false
	fmt.Fprintln(out, "Local plugins applied successfully.")
	return nil
}

func validatePluginMounts(c *Config) error {
	raw, err := captureCompose(c, "config", "--format", "json")
	if err != nil {
		return err
	}
	var model struct {
		Services map[string]struct {
			Volumes []struct {
				Source, Target string
				ReadOnly       bool `json:"read_only"`
			}
			Environment map[string]string
		}
	}
	if err = json.Unmarshal([]byte(raw), &model); err != nil {
		return err
	}
	services := []string{"php"}
	if c.Web.Server == "nginx" {
		services = append(services, "web")
	}
	for _, svc := range services {
		for _, m := range c.PluginMounts {
			found := false
			for _, v := range model.Services[svc].Volumes {
				if v.Target == m.Target && filepath.Clean(v.Source) == c.file(".wpdev", "plugins", m.ID) && v.ReadOnly {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("%s is missing the managed mount for %s; update your custom Compose template with wpdev templates update", svc, m.ID)
			}
		}
	}
	if model.Services["php"].Environment["WPDEV_LOCAL"] != "1" {
		return fmt.Errorf("local plugins require WPDEV_LOCAL=1")
	}
	for _, name := range []string{"wpdev-local.php", "remote-media.php"} {
		found := false
		for _, v := range model.Services["php"].Volumes {
			if v.Target == "/opt/wpdev/"+name && filepath.Clean(v.Source) == c.file(".wpdev", "generated", name) && v.ReadOnly {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("php is missing the %s local helper mount; review custom templates with wpdev templates update", name)
		}
	}
	return nil
}
