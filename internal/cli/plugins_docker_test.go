package cli

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

const dudeSource = "https://github.com/digitoimistodude/dude-load-media-from-remote/archive/refs/heads/main.zip"

func pluginFixtureZIP(t *testing.T, c *Config, version, body string) string {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, err := z.Create("fixture-main/fixture.php")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "<?php\n/*\nPlugin Name: wpdev fixture\nVersion: %s\n*/\n%s\n", version, body)
	f, err = z.Create("fixture-main/asset.txt")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, "fixture-static-"+version)
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(c.file(c.Web.Docroot, "fixture.zip"), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return c.URL() + "fixture.zip"
}

func TestDockerPlugins(t *testing.T) {
	dockerTests(t)
	for _, tc := range []struct {
		server, mode string
		tls          bool
	}{{"apache", "single", true}, {"nginx", "single", false}, {"apache", "subdirectory", false}, {"nginx", "subdirectory", true}, {"apache", "subdomain", true}, {"nginx", "subdomain", false}} {
		t.Run(fmt.Sprintf("%s-%s-%t", tc.server, tc.mode, tc.tls), func(t *testing.T) {
			c := dockerConfig(t, tc.server, "8.5")
			c.TLS.Enabled = tc.tls
			c.Multisite.Enabled = tc.mode != "single"
			if c.Multisite.Enabled {
				c.Multisite.Mode = tc.mode
			}
			if tc.mode == "subdomain" {
				c.Web.Docroot = "."
			}
			if err := os.MkdirAll(c.file(c.Web.Docroot), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.tls {
				t.Setenv("CAROOT", c.file(".wpdev", "certs"))
				root, ca, key := testCA(t)
				atomicWrite(c.file(".wpdev", "certs", "rootCA.pem"), root, 0o644)
				for _, item := range []struct{ stem, host string }{{c.Domain, c.Domain}, {"_wildcard." + c.Domain, "*." + c.Domain}} {
					cert, priv := testLeaf(t, ca, key, item.host, time.Now().Add(90*24*time.Hour))
					atomicWrite(c.file(".wpdev", "certs", item.stem+".pem"), cert, 0o644)
					atomicWrite(c.file(".wpdev", "certs", item.stem+"-key.pem"), priv, 0o600)
				}
				if err := tlsFingerprint(c); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveConfig(c.ConfigPath, c); err != nil {
				t.Fatal(err)
			}
			if err := renderTemplates(c); err != nil {
				t.Fatal(err)
			}
			dockerRun(t, c, "up", "-d", "--build", "--wait", "--wait-timeout", "180")
			version := os.Getenv("WPDEV_WP_VERSION")
			if version == "" {
				version = "latest"
			}
			dockerRun(t, c, wpArgs(c, "core", "download", "--force", "--version="+version)...)
			dockerRun(t, c, wpArgs(c, "config", "create", "--dbname=wordpress", "--dbuser=wp", "--dbpass=secret", "--dbhost=db", "--skip-check")...)
			args := []string{"core", "install"}
			if c.Multisite.Enabled {
				args = []string{"core", "multisite-install", "--base=/"}
				if tc.mode == "subdomain" {
					args = append(args, "--subdomains")
				}
			}
			args = append(args, "--url="+c.URL(), "--title=Plugins", "--admin_user=admin", "--admin_password=integration-password", "--admin_email=admin@example.test", "--skip-email")
			dockerRun(t, c, wpArgs(c, args...)...)
			if c.Multisite.Enabled {
				dockerRun(t, c, wpArgs(c, "site", "create", "--slug=child", "--title=Child", "--email=admin@example.test")...)
				if err := syncMultisite(c, true); err != nil {
					t.Fatal(err)
				}
			}
			m := mediaMap{MainID: 1, Sites: map[int]mediaSite{1: {Home: "https://source.example", SiteURL: "https://source.example/wp", Uploads: "https://source.example/wp/wp-content/uploads", LocalHome: strings.TrimRight(c.URL(), "/")}}}
			if c.Multisite.Enabled {
				m.Sites[2] = mediaSite{Home: "https://customer.example", SiteURL: "https://customer.example/wp", Uploads: "https://cdn.example/customer/uploads/sites/2", LocalHome: strings.TrimRight(c.URL(), "/") + "/child"}
			}
			if err := writeMediaMap(c, m); err != nil {
				t.Fatal(err)
			}
			g, err := loadGlobalPlugins()
			if err != nil {
				t.Fatal(err)
			}
			g.Plugins["dude-load-media-from-remote"] = pluginSpec{Source: dudeSource, Integration: "remote-media"}
			if err = g.save(); err != nil {
				t.Fatal(err)
			}
			oldCfg := cfgFile
			cfgFile = c.ConfigPath
			t.Cleanup(func() { cfgFile = oldCfg })
			if err = startCmd.RunE(startCmd, nil); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(c.file(".wpdev", "plugins-state.json"))
			if err = applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(c.file(".wpdev", "plugins-state.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("repeat apply changed installed state")
			}
			check := `$ids=is_multisite()?[1,2]:[1];foreach($ids as $blog){if(is_multisite())switch_to_blog($blog);$u=wp_get_upload_dir();$id=wp_insert_attachment(['post_title'=>'Missing media','post_status'=>'inherit','post_mime_type'=>'image/jpeg']);update_attached_file($id,$u['basedir'].'/wpdev-missing.jpg');$expected=$blog===1?'https://source.example/wp/wp-content/uploads/wpdev-missing.jpg':'https://cdn.example/customer/uploads/sites/2/wpdev-missing.jpg';$actual=wp_get_attachment_url($id);if($actual!==$expected){throw new RuntimeException($actual.' != '.$expected);} $src=apply_filters('wp_calculate_image_srcset',[100=>['url'=>$u['baseurl'].'/wpdev-missing.jpg','descriptor'=>'w','value'=>100]],[100,100],$u['baseurl'].'/wpdev-missing.jpg',[],$id);if($src[100]['url']!==$expected){throw new RuntimeException('srcset mismatch');}$js=apply_filters('wp_prepare_attachment_for_js',['url'=>$u['baseurl'].'/wpdev-missing.jpg','sizes'=>['thumbnail'=>['url'=>$u['baseurl'].'/wpdev-missing.jpg']]],get_post($id),[]);if($js['url']!==$expected||$js['sizes']['thumbnail']['url']!==$expected){throw new RuntimeException('media library mismatch');}wp_mkdir_p($u['basedir']);file_put_contents($u['basedir'].'/wpdev-missing.jpg','local');if(wp_get_attachment_url($id)!==$u['baseurl'].'/wpdev-missing.jpg'){throw new RuntimeException('local media was replaced');}unlink($u['basedir'].'/wpdev-missing.jpg');if(is_multisite())restore_current_blog();}if(getenv('REMOTE_MEDIA_URL')!=='https://source.example'){throw new RuntimeException('blog context was not restored');}echo 'media-ok';`
			dockerRun(t, c, wpArgs(c, "eval", check)...)
			// Exercise SAPI getenv, including Nginx/FPM, independently of WP-CLI.
			probe := `<?php require __DIR__.'/wp-load.php';header('Content-Type: application/json');echo json_encode([getenv('WP_ENV'),getenv('WP_HOME'),getenv('REMOTE_MEDIA_URL')]);`
			atomicWrite(c.file(c.Web.Docroot, "media-probe.php"), []byte(probe), 0o644)
			response := dockerRun(t, c, "exec", "-T", "php", "curl", "--fail", "-sS", c.URL()+"media-probe.php")
			if !strings.Contains(response, "development") || !strings.Contains(response, "source.example") {
				t.Fatal(response)
			}
			static := dockerRun(t, c, "exec", "-T", "php", "curl", "--fail", "-sS", c.URL()+"wp-content/plugins/dude-load-media-from-remote/LICENSE")
			if !strings.Contains(static, "GNU") {
				t.Fatal("plugin static mount missing")
			}
			if tc.server == "apache" && tc.mode == "single" {
				testPluginLifecycle(t, c, g)
			}
		})
	}
}

func testPluginLifecycle(t *testing.T, c *Config, g *globalPlugins) {
	t.Helper()
	body := `register_activation_hook(__FILE__,function(){update_option('wpdev_fixture_activated',(int)get_option('wpdev_fixture_activated',0)+1);});`
	url := pluginFixtureZIP(t, c, "1.0", body)
	g.Plugins["fixture"] = pluginSpec{Source: url}
	if err := g.save(); err != nil {
		t.Fatal(err)
	}
	if err := applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	dockerRun(t, c, wpArgs(c, "eval", `if(get_option('wpdev_fixture_activated')!=1)throw new RuntimeException('activation did not run');`)...)
	pluginFixtureZIP(t, c, "2.0", body)
	if err := applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, _ := readPluginState(c)
	if s.Plugins["fixture"].Version != "1.0" {
		t.Fatal("automatic update")
	}
	if err := applyPlugins(c, pluginApplyOptions{Update: true, ID: "fixture"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, _ = readPluginState(c)
	if s.Plugins["fixture"].Version != "2.0" {
		t.Fatal(s)
	}
	if err := applyPlugins(c, pluginApplyOptions{Update: true, ID: "fixture"}, io.Discard); err != nil {
		t.Fatal("identical package update", err)
	}
	asset := dockerRun(t, c, "exec", "-T", "php", "curl", "--fail", "-sS", c.URL()+"wp-content/plugins/fixture/asset.txt")
	if asset != "fixture-static-2.0" {
		t.Fatal("unchanged package lost its bind mount", asset)
	}
	dockerRun(t, c, wpArgs(c, "eval", `if(get_option('wpdev_fixture_activated')!=1)throw new RuntimeException('activation repeated');`)...)
	// A missing cache must be restored even when its saved digest is identical.
	if err := os.RemoveAll(c.file(".wpdev", "plugins", "fixture")); err != nil {
		t.Fatal(err)
	}
	if err := applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
		t.Fatal("repair missing cache", err)
	}
	asset = dockerRun(t, c, "exec", "-T", "php", "curl", "--fail", "-sS", c.URL()+"wp-content/plugins/fixture/asset.txt")
	if asset != "fixture-static-2.0" {
		t.Fatal("repaired cache lost its bind mount", asset)
	}
	pluginFixtureZIP(t, c, "3.0", `update_option('wpdev_bad_update','partial');throw new RuntimeException('fixture update failure');`)
	if err := applyPlugins(c, pluginApplyOptions{Update: true, ID: "fixture"}, io.Discard); err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatal("failed update was not restored", err)
	}
	s, _ = readPluginState(c)
	if s.Plugins["fixture"].Version != "2.0" {
		t.Fatal(s)
	}
	dockerRun(t, c, wpArgs(c, "eval", `if(get_option('wpdev_bad_update',false)!==false)throw new RuntimeException('partial DB mutation survived');`)...)
	atomicWrite(c.file(c.Web.Docroot, "fixture.zip"), []byte("broken zip"), 0o644)
	if err := applyPlugins(c, pluginApplyOptions{Update: true, ID: "fixture"}, io.Discard); err == nil {
		t.Fatal("invalid ZIP accepted")
	}
	delete(g.Plugins, "fixture")
	if err := g.save(); err != nil {
		t.Fatal(err)
	}
	if err := applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, _ = readPluginState(c)
	if s.Plugins["fixture"].Enabled {
		t.Fatal("removed plugin enabled")
	}
	if _, err := os.Stat(c.file(".wpdev", "plugins", "fixture", "fixture.php")); err != nil {
		t.Fatal("cache deleted", err)
	}
	pluginFixtureZIP(t, c, "1.0", `register_activation_hook(__FILE__,function(){update_option('wpdev_partial_activation','bad');throw new RuntimeException('activation fixture failure');});`)
	g.Plugins["activation-failure"] = pluginSpec{Source: url}
	if err := g.save(); err != nil {
		t.Fatal(err)
	}
	if err := applyPlugins(c, pluginApplyOptions{}, io.Discard); err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatal("activation failure not restored", err)
	}
	dockerRun(t, c, wpArgs(c, "eval", `if(get_option('wpdev_partial_activation',false)!==false)throw new RuntimeException('partial activation survived');`)...)
	delete(g.Plugins, "activation-failure")
	// Borrowed project plugins are activated, but never mounted or retired.
	owned := c.file(c.Web.Docroot, "wp-content", "plugins", "owned", "main.php")
	ownedCode := []byte("<?php\n/* Plugin Name: Project-owned fixture */\n")
	if err := atomicWrite(owned, ownedCode, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(c.file(c.Web.Docroot, "wp-content", "plugins", "single-owned.php"), ownedCode, 0o644); err != nil {
		t.Fatal(err)
	}
	g.Plugins["owned"] = pluginSpec{Source: "owned"}
	g.Plugins["single-owned"] = pluginSpec{Source: "single-owned"}
	if err := g.save(); err != nil {
		t.Fatal(err)
	}
	if err := applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, _ = readPluginState(c)
	if s.Plugins["owned"].Managed || s.Plugins["single-owned"].Managed {
		t.Fatal("borrowed plugin taken over")
	}
	delete(g.Plugins, "owned")
	delete(g.Plugins, "single-owned")
	if err := g.save(); err != nil {
		t.Fatal(err)
	}
	if err := applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	dockerRun(t, c, wpArgs(c, "plugin", "is-active", "owned")...)
	dockerRun(t, c, wpArgs(c, "plugin", "is-active", "single-owned")...)
	if actual, _ := os.ReadFile(owned); !bytes.Equal(actual, ownedCode) {
		t.Fatal("project file changed")
	}
	c.RemoteMedia.URL = "https://override.example"
	if err := applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	dockerRun(t, c, wpArgs(c, "eval", `if(getenv('REMOTE_MEDIA_URL')!=='https://override.example')throw new RuntimeException('override not applied');`)...)
	no := false
	c.RemoteMedia.Enabled = &no
	if err := applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	dockerRun(t, c, wpArgs(c, "eval", `if(getenv('REMOTE_MEDIA_URL'))throw new RuntimeException('disabled media still enabled');`)...)
	c.RemoteMedia = RemoteMediaCfg{}
	if err := applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// A plugin failure after import must preserve the newly published DB.
	dockerRun(t, c, wpArgs(c, "plugin", "deactivate", "dude-load-media-from-remote")...)
	dockerRun(t, c, wpArgs(c, "option", "update", "wpdev_import_marker", "from-dump")...)
	dump, err := dumpDatabase(c)
	if err != nil {
		t.Fatal(err)
	}
	dockerRun(t, c, wpArgs(c, "option", "update", "wpdev_import_marker", "old-local")...)
	dockerRun(t, c, wpArgs(c, "plugin", "activate", "dude-load-media-from-remote")...)
	g.Plugins["missing-fixture"] = pluginSpec{Source: "missing-fixture"}
	if err = g.save(); err != nil {
		t.Fatal(err)
	}
	original := executeProcess
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		if strings.Contains(strings.Join(cmd.Args, " "), "plugin install missing-fixture") {
			return fmt.Errorf("simulated interrupted plugin transfer")
		}
		return original(cmd)
	})
	command := &cobra.Command{}
	command.SetOut(io.Discard)
	err = localizeDatabase(command, c, dump, importOptions{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "database was localized successfully") {
		t.Fatal("missing separate plugin failure", err)
	}
	dockerRun(t, c, wpArgs(c, "eval", `if(get_option('wpdev_import_marker')!=='from-dump')throw new RuntimeException('successful import was rolled back');`)...)
	delete(g.Plugins, "missing-fixture")
	if err = g.save(); err != nil {
		t.Fatal(err)
	}
	if err = applyPlugins(c, pluginApplyOptions{}, io.Discard); err != nil {
		t.Fatal("retry after post-import failure", err)
	}
	dockerRun(t, c, wpArgs(c, "plugin", "is-active", "dude-load-media-from-remote")...)
	if matches, _ := filepath.Glob(c.file(".wpdev", ".plugin-stage-*")); len(matches) > 0 {
		t.Fatal("staging directories leaked", matches)
	}
}
