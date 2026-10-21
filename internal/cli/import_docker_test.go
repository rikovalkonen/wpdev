package cli

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerImport(t *testing.T) {
	dockerTests(t)
	for _, scenario := range []struct {
		mode, server, engine string
		tls                  bool
	}{
		{"subdirectory", "apache", "mariadb", true},
		{"subdomain", "nginx", "mariadb", false},
		{"mapped", "nginx", "mariadb", false},
		{"single", "nginx", "mysql", false},
	} {
		t.Run(scenario.mode, func(t *testing.T) {
			c := dockerConfig(t, scenario.server, "8.5")
			c.Domain = "ope.nuortennyt.fi"
			c.Database.Engine = scenario.engine
			if scenario.engine == "mysql" {
				c.Database.Version = "8.4"
			}
			if scenario.mode == "subdomain" {
				c.Web.Docroot = "."
			}
			c.Multisite.Enabled = scenario.mode != "single"
			c.Multisite.Mode = "subdirectory"
			if scenario.mode == "subdomain" {
				c.Multisite.Mode = "subdomain"
			}
			if scenario.mode == "subdomain" {
				c.Multisite.BasePath = "/"
			}
			os.MkdirAll(c.file(c.Web.Docroot), 0o755)
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
			dockerRun(t, c, wpArgs(c, "config", "create", "--dbname=wordpress", "--dbuser=wp", "--dbpass=secret", "--dbhost=db", "--dbprefix=prod_", "--skip-check")...)
			args := []string{"core", "install"}
			if c.Multisite.Enabled {
				args = []string{"core", "multisite-install", "--base=/"}
			}
			if scenario.mode == "subdomain" {
				args = append(args, "--subdomains")
			}
			args = append(args, "--url="+c.URL(), "--title=Import fixture", "--admin_user=admin", "--admin_password=integration-password", "--admin_email=admin@example.test", "--skip-email")
			dockerRun(t, c, wpArgs(c, args...)...)
			if c.Multisite.Enabled {
				dockerRun(t, c, wpArgs(c, "site", "create", "--slug=ohjeet", "--title=Ohjeet", "--email=admin@example.test")...)
			}
			child := "http://ope.nuortennyt.fi"
			if c.Multisite.Enabled {
				child += "/ohjeet"
			}
			if scenario.mode == "subdomain" {
				child = "http://ohjeet.ope.nuortennyt.fi"
			}
			if scenario.mode == "mapped" {
				child = "http://customer.example"
				dockerRun(t, c, wpArgs(c, "eval", `global $wpdb;$wpdb->query("UPDATE prod_blogs SET domain='customer.example',path='/' WHERE blog_id=2");$wpdb->query("UPDATE prod_2_options SET option_value='http://customer.example' WHERE option_name IN ('home','siteurl')");`)...)
			}
			seed := `global $wpdb;$value=['url'=>` + phpLiteral(child+`/article`) + `,'mail'=>'person@ope.nuortennyt.fi','json'=>json_encode(['url'=>` + phpLiteral(child+`/image.png`) + `])];update_option('wpdev_import_fixture',$value);$wpdb->query("CREATE TABLE prod_extra (id int PRIMARY KEY, data longtext)");$wpdb->insert('prod_extra',['id'=>1,'data'=>serialize($value)]);$id=wp_insert_post(['post_title'=>'Import post','post_status'=>'publish']);$wpdb->update($wpdb->posts,['guid'=>'http://ope.nuortennyt.fi/unchanged-guid'],['ID'=>$id]);`
			dockerRun(t, c, wpArgs(c, "eval", seed)...)
			dockerRun(t, c, wpArgs(c, "eval", `global $wpdb;$wpdb->query("CREATE TABLE third_party_data (value int)");$wpdb->query("INSERT INTO third_party_data VALUES (42)");`)...)
			dump, err := dumpDatabase(c)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.mode == "subdirectory" {
				f, err := os.Create(dump + ".gz")
				if err != nil {
					t.Fatal(err)
				}
				gz := gzip.NewWriter(f)
				in, _ := os.Open(dump)
				if _, err = io.Copy(gz, in); err != nil {
					t.Fatal(err)
				}
				in.Close()
				gz.Close()
				f.Close()
				dump += ".gz"
			}
			_, client := dbTools(c)
			dockerRun(t, c, "exec", "-T", "db", client, "-uroot", "-proot", "wordpress", "-e", "CREATE TABLE local_sentinel (value int); INSERT INTO local_sentinel VALUES (42);")
			c.Domain = "masterportaali.test"
			c.TLS.Enabled = scenario.tls
			if scenario.tls {
				t.Setenv("CAROOT", c.file(".wpdev", "certs"))
				root, ca, key := testCA(t)
				atomicWrite(c.file(".wpdev", "certs", "rootCA.pem"), root, 0o644)
				for _, item := range []struct{ stem, host string }{{c.Domain, c.Domain}, {"_wildcard." + c.Domain, "*." + c.Domain}} {
					cert, priv := testLeaf(t, ca, key, item.host, time.Now().Add(90*24*time.Hour))
					atomicWrite(c.file(".wpdev", "certs", item.stem+".pem"), cert, 0o644)
					atomicWrite(c.file(".wpdev", "certs", item.stem+"-key.pem"), priv, 0o600)
				}
			}
			if err = saveConfig(c.ConfigPath, c); err != nil {
				t.Fatal(err)
			}
			opts := importOptions{Yes: true}
			if scenario.mode == "single" {
				broken := c.file("broken.sql")
				atomicWrite(broken, []byte("CREATE TABLE half_imported (id int); THIS IS NOT SQL;"), 0o600)
				if err = localizeDatabase(importTestCommand(), c, broken, opts); err == nil {
					t.Fatal("invalid SQL accepted")
				}
				out := dockerRun(t, c, "exec", "-T", "db", client, "-uroot", "-proot", "wordpress", "-Nse", "SELECT value FROM local_sentinel")
				if !strings.Contains(out, "42") {
					t.Fatal("invalid import touched live DB")
				}
				// First import into a prepared directory with no generated Compose.
				fresh := dockerConfig(t, "nginx", "8.5")
				fresh.Database.Engine = "mysql"
				fresh.Database.Version = "8.4"
				fresh.Domain = "fresh-import.test"
				if out, err := exec.Command("cp", "-R", c.file(c.Web.Docroot), fresh.file(fresh.Web.Docroot)).CombinedOutput(); err != nil {
					t.Fatalf("copy WordPress fixture: %v %s", err, out)
				}
				if err = saveConfig(fresh.ConfigPath, fresh); err != nil {
					t.Fatal(err)
				}
				if err = localizeDatabase(importTestCommand(), fresh, dump, opts); err != nil {
					t.Fatal("fresh project import", err)
				}
			}
			if scenario.mode == "mapped" {
				opts.Map = c.file("mapping.yml")
				atomicWrite(opts.Map, []byte("sites:\n  2: http://masterportaali.test/asiakas/\n"), 0o644)
			}
			if scenario.mode == "subdirectory" {
				before, _ := os.ReadFile(c.file(c.Web.Docroot, "wp-config.php"))
				yamlBefore, _ := os.ReadFile(c.ConfigPath)
				dry := opts
				dry.DryRun = true
				if err = localizeDatabase(importTestCommand(), c, dump, dry); err != nil {
					t.Fatal(err)
				}
				after, _ := os.ReadFile(c.file(c.Web.Docroot, "wp-config.php"))
				yamlAfter, _ := os.ReadFile(c.ConfigPath)
				if !bytes.Equal(before, after) || !bytes.Equal(yamlBefore, yamlAfter) {
					t.Fatal("dry run changed project")
				}
				out := dockerRun(t, c, "exec", "-T", "db", client, "-uroot", "-proot", "wordpress", "-Nse", "SELECT value FROM local_sentinel")
				if !strings.Contains(out, "42") {
					t.Fatal("dry run changed DB")
				}
				// Inject one publication failure after DB replacement. Real Docker
				// performs the backup and restore, so this proves rollback behavior.
				old := executeProcess
				injected := false
				executeProcess = func(cmd *exec.Cmd) error {
					if !injected && strings.Contains(strings.Join(cmd.Args, " "), " up -d --build --wait") && cmd.Dir == c.Root {
						injected = true
						return fmt.Errorf("injected failure starting localized stack")
					}
					return old(cmd)
				}
				err = localizeDatabase(importTestCommand(), c, dump, opts)
				executeProcess = old
				if err == nil || !injected || !strings.Contains(err.Error(), "restored") {
					t.Fatalf("expected rollback: %v", err)
				}
				after, _ = os.ReadFile(c.file(c.Web.Docroot, "wp-config.php"))
				if !bytes.Equal(before, after) {
					t.Fatal("rollback did not restore config")
				}
				out = dockerRun(t, c, "exec", "-T", "db", client, "-uroot", "-proot", "wordpress", "-Nse", "SELECT value FROM local_sentinel")
				if !strings.Contains(out, "42") {
					t.Fatal("rollback did not restore DB")
				}
			}
			if err = localizeDatabase(importTestCommand(), c, dump, opts); err != nil {
				t.Fatal(err)
			}
			expected := c.scheme() + "://masterportaali.test"
			if scenario.mode == "subdirectory" {
				expected += "/ohjeet"
			}
			if scenario.mode == "subdomain" {
				expected = c.scheme() + "://ohjeet.masterportaali.test"
			}
			if scenario.mode == "mapped" {
				expected += "/asiakas"
			}
			check := `$v=get_option('wpdev_import_fixture');if($v['url']!==` + phpLiteral(expected+"/article") + `||$v['mail']!=='person@ope.nuortennyt.fi'||json_decode($v['json'],true)['url']!==` + phpLiteral(expected+"/image.png") + `){var_export($v);exit(1);}global $wpdb;$extra=unserialize($wpdb->get_var("SELECT data FROM prod_extra WHERE id=1"));if($extra!==$v){var_export($extra);exit(2);}if(!$wpdb->get_var("SELECT ID FROM prod_posts WHERE guid='http://ope.nuortennyt.fi/unchanged-guid'")){exit(3);}`
			dockerRun(t, c, wpArgs(c, "eval", check)...)
			dockerRun(t, c, wpArgs(c, "eval", `global $wpdb;if($wpdb->get_var("SELECT value FROM third_party_data")!=42){exit(1);}`)...)
			dockerRun(t, c, "exec", "-T", "php", "sh", "-c", `set -eu; curl -sS -c /tmp/import-cookies "$1/wp-login.php" >/dev/null; curl -sS -L -b /tmp/import-cookies -c /tmp/import-cookies --data-urlencode log=admin --data-urlencode pwd=integration-password --data-urlencode testcookie=1 "$1/wp-login.php" -o /tmp/import-login; grep -q wordpress_logged_in /tmp/import-cookies; curl -fsS -L -b /tmp/import-cookies "$1/wp-admin/" | grep -q wpadminbar`, "sh", expected)
			if c.TLS.Enabled {
				dockerRun(t, c, wpArgs(c, "eval", `$r=wp_remote_get(`+phpLiteral(expected)+`);if(is_wp_error($r)){WP_CLI::error($r->get_error_message());}if(wp_remote_retrieve_response_code($r)!==200){exit(1);}`)...)
			}
			loaded, err := loadConfig(c.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Multisite.Enabled != (scenario.mode != "single") {
				t.Fatal("YAML not synchronized")
			}
			if err = startProject(loaded, false); err != nil {
				t.Fatal(err)
			}
			if scenario.mode == "single" {
				dry := opts
				dry.DryRun = true
				if err = localizeDatabase(importTestCommand(), loaded, "", dry); err != nil {
					t.Fatal("localize existing DB", err)
				}
			}
		})
	}
}

func TestImportRollbackReadFailure(t *testing.T) {
	c, _ := importFixture()
	c.Root = t.TempDir()
	c.ConfigPath = filepath.Join(c.Root, ".wpdev.yml")
	atomicWrite(c.ConfigPath, []byte("settings"), 0o644)
	os.MkdirAll(c.file(".wpdev", "generated"), 0o755)
	os.Symlink("/tmp", c.file(".wpdev", "generated", "external"))
	if _, err := snapshotImport(c); err == nil {
		t.Fatal("snapshot followed symlink")
	}
}
