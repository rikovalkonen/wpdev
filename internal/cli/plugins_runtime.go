package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type pluginRecord struct {
	pluginSpec
	Version string `json:"version"`
	File    string `json:"file"`
	Digest  string `json:"digest,omitempty"`
	Managed bool   `json:"managed"`
	Enabled bool   `json:"enabled"`
}
type pluginState struct {
	Plugins map[string]pluginRecord `json:"plugins"`
}
type pluginMount struct{ ID, Source, Target string }

func readPluginState(c *Config) (pluginState, error) {
	s := pluginState{Plugins: map[string]pluginRecord{}}
	b, err := os.ReadFile(c.file(".wpdev", "plugins-state.json"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("plugin state: %w", err)
	}
	if s.Plugins == nil {
		s.Plugins = map[string]pluginRecord{}
	}
	for id, r := range s.Plugins {
		if err = validatePluginSpec(id, r.pluginSpec); err != nil {
			return s, err
		}
		validFile := strings.HasPrefix(r.File, id+"/") || !r.Managed && r.File == id+".php"
		if !relativePath(r.File) || !validFile || !strings.HasSuffix(r.File, ".php") {
			return s, fmt.Errorf("invalid plugin file for %s", id)
		}
	}
	return s, nil
}
func writePluginState(c *Config, s pluginState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(c.file(".wpdev", "plugins-state.json"), b, 0o600)
}
func directoryDigest(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("plugin contains a symlink or special file: %s", p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", rel, info.Size())
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// Docker may leave an empty mount-point directory in the project. Anything
// nonempty or symlinked belongs to the user and must never be hidden by a mount.
func projectPluginExists(c *Config, id string) (bool, error) {
	p := c.file(c.Web.Docroot, "wp-content", "plugins", id)
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	entries, err := os.ReadDir(p)
	return len(entries) > 0, err
}
func loadPluginRuntime(c *Config) error {
	s, err := readPluginState(c)
	if err != nil {
		return err
	}
	c.PluginMounts = nil
	activeMedia := false
	for _, id := range sortedPluginIDs(s.Plugins) {
		r := s.Plugins[id]
		if !r.Enabled {
			continue
		}
		if r.Managed {
			p := c.file(".wpdev", "plugins", id)
			info, e := os.Lstat(p)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return e
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("managed plugin directory must not be a symlink: %s", p)
			}
			if exists, e := projectPluginExists(c, id); e != nil {
				return e
			} else if exists {
				return fmt.Errorf("project plugin %s conflicts with wpdev's managed copy; disable it in local_plugins.disabled before choosing which copy to keep", id)
			}
			c.PluginMounts = append(c.PluginMounts, pluginMount{id, "./.wpdev/plugins/" + id, c.containerDocroot() + "/wp-content/plugins/" + id})
		}
		if r.Integration == "remote-media" {
			activeMedia = true
		}
	}
	if activeMedia {
		if err = checkMediaHTAccess(c); err != nil {
			return err
		}
	}
	c.RemoteMediaConfigured = activeMedia
	m, err := readMediaMap(c)
	if err != nil {
		return err
	}
	b, err := json.Marshal(map[string]any{"configured": activeMedia, "active": activeMedia && enabled(c.RemoteMedia.Enabled), "map": m, "overrides": c.RemoteMedia})
	if err != nil {
		return err
	}
	c.RemoteMediaJSON = string(b)
	b, err = json.Marshal(s)
	if err != nil {
		return err
	}
	c.PluginFingerprint = hash(b)
	return nil
}

type installedPlugin struct {
	File      string `json:"file"`
	Version   string `json:"version"`
	Active    bool   `json:"active"`
	Network   bool   `json:"network"`
	Ambiguous bool   `json:"ambiguous"`
}

const pluginInventoryPHP = `require_once ABSPATH.'wp-admin/includes/plugin.php'; if (WP_PLUGIN_DIR !== ABSPATH.'wp-content/plugins' && WP_PLUGIN_DIR !== rtrim(ABSPATH,'/').'/wp-content/plugins') { throw new RuntimeException('Local plugins require the standard wp-content/plugins directory'); } $out=[];foreach(get_plugins() as $file=>$data){$id=str_contains($file,'/')?strtok($file,'/'):basename($file,'.php');if(isset($out[$id])){$out[$id]['ambiguous']=true;continue;}$out[$id]=['file'=>$file,'version'=>$data['Version'],'active'=>is_plugin_active($file),'network'=>is_plugin_active_for_network($file)];}echo json_encode((object)$out);`

func pluginInventory(c *Config) (map[string]installedPlugin, error) {
	out, err := captureWP(c, "--skip-plugins", "--skip-themes", "--skip-packages", "eval", pluginInventoryPHP)
	if err != nil {
		return nil, err
	}
	var inventory map[string]installedPlugin
	err = json.Unmarshal([]byte(out), &inventory)
	return inventory, err
}

// WP-CLI performs download, unpacking and plugin requirement checks, but its
// installation directory is isolated from both the active plugin mounts and
// project-owned plugins. Nothing is activated in this staging directory.
func stagePlugin(c *Config, id string, spec pluginSpec, out io.Writer) (string, pluginRecord, func(), error) {
	r := pluginRecord{pluginSpec: spec, Managed: true, Enabled: true}
	emptyCleanup := func() {}
	if err := os.MkdirAll(c.file(".wpdev"), 0o755); err != nil {
		return "", r, emptyCleanup, err
	}
	dir, err := os.MkdirTemp(c.file(".wpdev"), ".plugin-stage-")
	if err != nil {
		return "", r, emptyCleanup, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	if err = os.Chmod(dir, 0o755); err != nil {
		return "", r, cleanup, err
	}
	pluginDir := filepath.Join(dir, "plugins")
	if err = os.Mkdir(pluginDir, 0o755); err != nil {
		return "", r, cleanup, err
	}
	rel, err := filepath.Rel(c.Root, pluginDir)
	if err != nil {
		return "", r, cleanup, err
	}
	containerDir := "/var/www/html/" + filepath.ToSlash(rel)
	boot := "--exec=define('WP_PLUGIN_DIR'," + phpLiteral(containerDir) + ");"
	args := []string{"--skip-plugins", "--skip-themes", "--skip-packages", boot, "plugin", "install", spec.Source}
	if err = runWP(c, nil, out, args...); err != nil {
		return "", r, cleanup, err
	}
	list, err := captureWP(c, "--skip-plugins", "--skip-themes", "--skip-packages", boot, "eval", `require_once ABSPATH.'wp-admin/includes/plugin.php';$p=get_plugins();echo json_encode((object)$p);`)
	if err != nil {
		return "", r, cleanup, err
	}
	var headers map[string]struct{ Version string }
	if err = json.Unmarshal([]byte(list), &headers); err != nil {
		return "", r, cleanup, err
	}
	if len(headers) != 1 {
		return "", r, cleanup, fmt.Errorf("plugin %s archive must contain exactly one plugin entrypoint", id)
	}
	var entry string
	for file, header := range headers {
		entry = file
		r.Version = header.Version
	}
	if !relativePath(entry) || !strings.HasSuffix(entry, ".php") {
		return "", r, cleanup, fmt.Errorf("invalid plugin entrypoint %q", entry)
	}
	parts := strings.SplitN(entry, "/", 2)
	var root string
	if len(parts) == 2 {
		root = filepath.Join(pluginDir, parts[0])
		r.File = id + "/" + parts[1]
	} else {
		root = filepath.Join(dir, "single")
		if err = os.Mkdir(root, 0o755); err != nil {
			return "", r, cleanup, err
		}
		if err = os.Rename(filepath.Join(pluginDir, entry), filepath.Join(root, entry)); err != nil {
			return "", r, cleanup, err
		}
		r.File = id + "/" + entry
	}
	r.Digest, err = directoryDigest(root)
	return root, r, cleanup, err
}
