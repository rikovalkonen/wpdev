package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobalPlugins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	g, err := loadGlobalPlugins()
	if err != nil || len(g.Plugins) != 0 {
		t.Fatal(g, err)
	}
	if err = atomicWrite(g.path, []byte("# global settings\nfuture: keep\nplugins:\n  old:\n    source: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err = loadGlobalPlugins()
	if err != nil {
		t.Fatal(err)
	}
	delete(g.Plugins, "old")
	g.Plugins["query-monitor"] = pluginSpec{Source: "query-monitor"}
	if err = g.save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(g.path)
	if strings.Contains(string(b), "old:") || !strings.Contains(string(b), "future: keep") || !strings.Contains(string(b), "# global settings") {
		t.Fatal(string(b))
	}
	g, err = loadGlobalPlugins()
	if err != nil || len(g.Plugins) != 1 {
		t.Fatal(g, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	p, err := pluginConfigPath()
	if err != nil || p != filepath.Join(home, ".config", "wpdev", "config.yml") {
		t.Fatal(p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if _, err = pluginConfigPath(); err == nil {
		t.Fatal("accepted relative XDG path")
	}
}
func TestPluginValidationAndOverrides(t *testing.T) {
	c := testConfig(t)
	g := &globalPlugins{Plugins: map[string]pluginSpec{"query-monitor": {Source: "query-monitor"}, "dude": {Source: "https://github.com/example/plugin/main.zip", Integration: "remote-media"}}}
	for id, spec := range g.Plugins {
		if err := validatePluginSpec(id, spec); err != nil {
			t.Fatal(err)
		}
	}
	for _, spec := range []pluginSpec{{Source: "http://example.com/plugin.zip"}, {Source: "https://user:pass@example.com/plugin.zip"}, {Source: "../plugin"}, {Source: "https://example.com/plugin.php"}, {Source: "plugin", Integration: "shell"}} {
		if validatePluginSpec("plugin", spec) == nil {
			t.Fatal("accepted", spec)
		}
	}
	if validatePluginSpec("../bad", pluginSpec{Source: "plugin"}) == nil {
		t.Fatal("unsafe ID")
	}
	c.LocalPlugins.Disabled = []string{"query-monitor"}
	if len(desiredPlugins(c, g)) != 1 {
		t.Fatal("disabled entry retained")
	}
	no := false
	c.LocalPlugins.Enabled = &no
	if len(desiredPlugins(c, g)) != 0 {
		t.Fatal("disabled list retained")
	}
	c.RemoteMedia.Sites = map[int]string{7: "https://source.example/child/"}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.RemoteMedia.Sites[0] = "https://source.example"
	if c.validate() == nil {
		t.Fatal("invalid site ID accepted")
	}
}
func TestPluginPreviewAndState(t *testing.T) {
	c := testConfig(t)
	g, _ := loadGlobalPlugins()
	g.Plugins["example"] = pluginSpec{Source: "example"}
	if err := g.save(); err != nil {
		t.Fatal(err)
	}
	fakeProcesses(t, func(cmd *exec.Cmd) error { t.Fatalf("preview executed %v", cmd.Args); return nil })
	var out bytes.Buffer
	if err := applyPlugins(c, pluginApplyOptions{DryRun: true}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "example") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(c.file(".wpdev")); !os.IsNotExist(err) {
		t.Fatal("preview wrote project files")
	}
	s := pluginState{Plugins: map[string]pluginRecord{"example": {pluginSpec: pluginSpec{Source: "example"}, File: "../../outside.php", Managed: true}}}
	if err := writePluginState(c, s); err != nil {
		t.Fatal(err)
	}
	if _, err := readPluginState(c); err == nil {
		t.Fatal("accepted unsafe plugin file")
	}
}
func TestPluginMountsAndProjectCollision(t *testing.T) {
	c := testConfig(t)
	r := pluginRecord{pluginSpec: pluginSpec{Source: "example"}, File: "example/main.php", Managed: true, Enabled: true}
	if err := atomicWrite(c.file(".wpdev", "plugins", r.File), []byte("<?php"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePluginState(c, pluginState{Plugins: map[string]pluginRecord{"example": r}}); err != nil {
		t.Fatal(err)
	}
	if err := loadPluginRuntime(c); err != nil {
		t.Fatal(err)
	}
	if len(c.PluginMounts) != 1 || c.PluginMounts[0].Target != "/var/www/html/wp/wp-content/plugins/example" {
		t.Fatal(c.PluginMounts)
	}
	if err := atomicWrite(c.file("wp", "wp-content", "plugins", "example", "custom.php"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadPluginRuntime(c); err == nil {
		t.Fatal("hid user plugin")
	}
}

func TestPluginOperationLock(t *testing.T) {
	c := testConfig(t)
	g, err := loadGlobalPlugins()
	if err != nil {
		t.Fatal(err)
	}
	g.Plugins["example"] = pluginSpec{Source: "example"}
	if err = g.save(); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(c.file(".wpdev", "import.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		t.Fatalf("locked operation executed %v", cmd.Args)
		return nil
	})
	var out bytes.Buffer
	if err = applyPlugins(c, pluginApplyOptions{}, &out); err == nil || !strings.Contains(err.Error(), "cannot acquire") {
		t.Fatal("concurrent operation not rejected", err)
	}
	if _, err = os.Stat(c.file(".wpdev", "import.lock")); err != nil {
		t.Fatal("removed another operation's lock", err)
	}
}

func TestMediaMapRelocalizationAndHTAccess(t *testing.T) {
	c, metadata := importFixture()
	c.Root = t.TempDir()
	p, err := makeImportPlan(c, metadata, importOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m := mediaMap{MainID: 1, Sites: map[int]mediaSite{1: {Home: "https://source.example", SiteURL: "https://source.example/wp", Uploads: "https://source.example/wp/wp-content/uploads", LocalHome: "https://masterportaali.test"}}}
	if err := writeMediaMap(c, m); err != nil {
		t.Fatal(err)
	}
	p.Source.Sites[0].Home = "https://masterportaali.test"
	incoming := mediaMap{MainID: 1, Sites: map[int]mediaSite{1: {Home: "https://masterportaali.test", Uploads: "https://masterportaali.test/wp-content/uploads"}}}
	got, err := planMediaMap(c, p, incoming)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sites[1].Home != "https://source.example" {
		t.Fatal(got)
	}
	if err = atomicWrite(c.file(c.Web.Docroot, ".htaccess"), []byte("# keep\nSetEnv REMOTE_MEDIA_URL https://old.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = checkMediaHTAccess(c); err == nil || !strings.Contains(err.Error(), "was not changed") {
		t.Fatal(err)
	}
	c.Web.Server = "nginx"
	if err = checkMediaHTAccess(c); err != nil {
		t.Fatal(err)
	}
}
func TestIgnoreUpgradeAndPluginSnapshot(t *testing.T) {
	c := testConfig(t)
	if err := atomicWrite(c.file(".gitignore"), []byte("# wpdev local artifacts\n/docker-compose.yml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := projectIgnore(c); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(c.file(".gitignore"))
	if !bytes.Contains(b, []byte("/.wpdev/plugins/")) {
		t.Fatal(string(b))
	}
	projectIgnore(c)
	after, _ := os.ReadFile(c.file(".gitignore"))
	if !bytes.Equal(b, after) {
		t.Fatal("ignore not idempotent")
	}
	p := c.file(".wpdev", "plugins", "example", "main.php")
	atomicWrite(p, []byte("original"), 0o644)
	s, err := snapshotManaged(c, "plugins", ".wpdev/plugins")
	if err != nil {
		t.Fatal(err)
	}
	atomicWrite(p, []byte("changed"), 0o644)
	atomicWrite(c.file(".wpdev", "plugins", "new", "main.php"), []byte("new"), 0o644)
	if err = s.restore(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != "original" {
		t.Fatal(string(b))
	}
	if _, err = os.Stat(c.file(".wpdev", "plugins", "new")); !os.IsNotExist(err) {
		t.Fatal("new plugin survived restore")
	}
}
