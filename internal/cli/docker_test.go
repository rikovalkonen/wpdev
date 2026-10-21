package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dockerTests(t *testing.T) {
	t.Helper()
	if os.Getenv("WPDEV_DOCKER_TESTS") != "1" {
		t.Skip("set WPDEV_DOCKER_TESTS=1 to run isolated Docker integration tests")
	}
}
func dockerConfig(t *testing.T, server, php string) *Config {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root, err := os.MkdirTemp("", "wpdev-it-")
	if err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.Root = root
	c.ConfigPath = c.file(".wpdev.yml")
	c.Name = "wpdev-test"
	c.Domain = "wpdev-integration.test"
	c.Web.Server = server
	c.Web.PHP = php
	c.Database.Portforward = ""
	t.Cleanup(func() {
		cmd := exec.Command("docker", composeArgs(c, "down", "--volumes", "--remove-orphans", "--rmi", "local")...)
		cmd.Dir = root
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Logf("cleanup failed: %v %s", e, out)
		}
		os.RemoveAll(root)
	})
	if err = saveConfig(c.ConfigPath, c); err != nil {
		t.Fatal(err)
	}
	override := "services:\n  caddy:\n    ports: !reset []\n"
	if err = atomicWrite(c.file("docker-compose.override.yml"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	return c
}
func dockerRun(t *testing.T, c *Config, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", composeArgs(c, args...)...)
	cmd.Dir = c.Root
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := out
		if len(tail) > 14000 {
			tail = tail[len(tail)-14000:]
		}
		t.Fatalf("docker compose %v: %v\n%s", args, err, tail)
	}
	return string(out)
}
func TestDockerBuild(t *testing.T) {
	dockerTests(t)
	for _, php := range []string{"8.5", "8.4", "8.3"} {
		for _, server := range []string{"apache", "nginx"} {
			t.Run(php+"-"+server, func(t *testing.T) {
				c := dockerConfig(t, server, php)

				// Only build checks use shared cache tags. Runtime projects keep
				// their own image names so concurrent suites cannot retag them.
				override := fmt.Sprintf("services:\n  php:\n    image: wpdev-integration:%s-%s\n  caddy:\n    ports: !reset []\n", php, server)
				if err := atomicWrite(c.file("docker-compose.override.yml"), []byte(override), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := renderTemplates(c); err != nil {
					t.Fatal(err)
				}
				dockerRun(t, c, "build", "php")
				out := dockerRun(t, c, "run", "--rm", "--no-deps", "php", "php", "-r", `foreach(['bcmath','exif','gd','intl','mysqli','soap','zip','imagick','redis','xdebug','Zend OPcache'] as $e){if(!extension_loaded($e)){fwrite(STDERR,"missing $e\n");exit(1);}}if(!gd_info()['AVIF Support']){exit(2);}echo PHP_VERSION;`)
				if !strings.Contains(out, php+".") {
					t.Fatal("wrong PHP version", out)
				}
				out = dockerRun(t, c, "run", "--rm", "--no-deps", "php", "wp", "--allow-root", "--version")
				if !strings.Contains(out, "2.12.0") {
					t.Fatal("wrong WP-CLI version", out)
				}
			})
		}
	}
}

func TestDockerRuntime(t *testing.T) {
	dockerTests(t)
	for _, server := range []string{"apache", "nginx"} {
		for _, mode := range []string{"single", "subdirectory", "subdomain"} {
			for _, tlsEnabled := range []bool{false, true} {
				label := server + "-" + mode + fmt.Sprintf("-tls%t", tlsEnabled)
				t.Run(label, func(t *testing.T) {
					c := dockerConfig(t, server, "8.5")
					c.TLS.Enabled = tlsEnabled
					if tlsEnabled {
						t.Setenv("CAROOT", c.file(".wpdev", "certs"))
					}
					c.Services.Redis = true
					if mode != "single" {
						c.Multisite.Enabled = true
						c.Multisite.Mode = mode
						if mode == "subdirectory" && !tlsEnabled {
							c.Multisite.BasePath = "/network/"
						}
					}
					if mode == "subdomain" {
						c.Web.Docroot = "."
					}
					if err := os.MkdirAll(c.file(c.Web.Docroot), 0o755); err != nil {
						t.Fatal(err)
					}
					if tlsEnabled {
						root, ca, key := testCA(t)
						if err := atomicWrite(c.file(".wpdev", "certs", "rootCA.pem"), root, 0o644); err != nil {
							t.Fatal(err)
						}
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
					dockerRun(t, c, "exec", "-T", "php", "php", "-r", `$r=new Redis();$r->connect('redis');if(!$r->ping()){exit(1);}`)
					version := os.Getenv("WPDEV_WP_VERSION")
					if version == "" {
						version = "latest"
					}
					dockerRun(t, c, wpArgs(c, "core", "download", "--force", "--version="+version)...)
					dockerRun(t, c, wpArgs(c, "config", "create", "--dbname=wordpress", "--dbuser=wp", "--dbpass=secret", "--dbhost=db", "--skip-check")...)
					// New core files cause the local MU-plugin mount to become active.
					if err := renderTemplates(c); err != nil {
						t.Fatal(err)
					}
					dockerRun(t, c, "up", "-d", "--wait", "--wait-timeout", "180")
					oldConfig := cfgFile
					cfgFile = c.ConfigPath
					t.Cleanup(func() { cfgFile = oldConfig })
					if mode == "single" || mode == "subdirectory" {
						dockerRun(t, c, wpArgs(c, "core", "install", "--url="+c.URL(), "--title=Integration", "--admin_user=admin", "--admin_password=integration-password", "--admin_email=admin@example.test", "--skip-email")...)
						if mode == "single" {
							rewrite := []string{"rewrite", "structure", "/%postname%/"}
							if server == "apache" {
								rewrite = append(rewrite, "--hard")
							}
							dockerRun(t, c, wpArgs(c, rewrite...)...)
						}
					}
					if mode != "single" {
						command := installNetworkCommand(mode == "subdirectory")
						command.SetOut(io.Discard)
						command.Flags().Set("title", "Integration")
						if mode == "subdomain" {
							command.Flags().Set("admin-user", "admin")
							command.Flags().Set("admin-email", "admin@example.test")
							command.Flags().Set("admin-password", "integration-password")
						}
						if err := command.RunE(command, nil); err != nil {
							t.Fatal(err)
						}
						// Exercise the native site command, including its automatic sync.
						var create *cobra.Command
						for _, cmd := range siteCmd.Commands() {
							if cmd.Name() == "create" {
								create = cmd
							}
						}
						if err := create.RunE(create, []string{"child"}); err != nil {
							t.Fatal(err)
						}
						if err := syncMultisite(c, true); err != nil {
							t.Fatal("repeat sync", err)
						}
						if mode == "subdirectory" {
							backups, _ := filepath.Glob(c.file(".wpdev", "db", "*.sql"))
							if len(backups) == 0 {
								t.Fatal("conversion missing DB backup")
							}
						}
					}
					if mode == "single" && server == "apache" && !tlsEnabled {
						if err := xdebugCmd.RunE(xdebugCmd, []string{"on"}); err != nil {
							t.Fatal(err)
						}
						out := dockerRun(t, c, "exec", "-T", "php", "php", "-r", `echo implode(',',xdebug_info('mode'));`)
						if !strings.Contains(out, "debug") {
							t.Fatal("Xdebug mode did not activate", out)
						}
						if err := xdebugCmd.RunE(xdebugCmd, []string{"off"}); err != nil {
							t.Fatal(err)
						}
						out = dockerRun(t, c, "exec", "-T", "php", "php", "-r", `echo implode(',',xdebug_info('mode'));`)
						if strings.TrimSpace(out) != "" {
							t.Fatal("Xdebug mode did not turn off", out)
						}
					}
					if server == "nginx" {
						dockerRun(t, c, "exec", "-T", "web", "nginx", "-t")
					} else {
						dockerRun(t, c, "exec", "-T", "php", "apachectl", "-t")
					}
					dockerRun(t, c, "exec", "-T", "caddy", "caddy", "validate", "--config", "/etc/caddy/Caddyfile")
					urls := []string{c.URL()}
					if mode == "subdirectory" {
						urls = append(urls, c.URL()+"child/")
					}
					if mode == "subdomain" {
						urls = append(urls, c.scheme()+"://child."+c.Domain+"/")
					}
					for _, url := range urls {
						headers := strings.ToLower(dockerRun(t, c, "exec", "-T", "php", "curl", "-sSI", "--max-time", "20", url+"wp-admin"))
						if tlsEnabled && (strings.Contains(headers, "location: http://") || strings.Contains(headers, ":80/wp-admin")) {
							t.Errorf("HTTPS redirect downgraded: %s", headers)
						}
						for _, suffix := range []string{"", "wp-json/", "wp-login.php", "wp-admin/", "wp-includes/css/dashicons.min.css"} {
							code := dockerRun(t, c, "exec", "-T", "php", "curl", "-sS", "-L", "--max-time", "20", "-o", "/dev/null", "-w", "%{http_code}", url+suffix)
							if strings.TrimSpace(code) != "200" {
								t.Errorf("%s got %s", url+suffix, code)
							}
						}
						dockerRun(t, c, "exec", "-T", "php", "php", "-r", `if(file_get_contents($argv[1])===false){exit(1);}`, url)
						dockerRun(t, c, wpArgs(c, "eval", `$r=wp_remote_get(`+phpLiteral(url)+`,['timeout'=>20]);if(is_wp_error($r)){WP_CLI::error($r->get_error_message());}if(wp_remote_retrieve_response_code($r)!==200){WP_CLI::error('HTTP '.wp_remote_retrieve_response_code($r));}`)...)
						// Real login with a cookie jar, followed by an authenticated admin page.
						dockerRun(t, c, "exec", "-T", "php", "sh", "-c", `set -eu; curl -sS -c /tmp/wpdev-cookies "$1/wp-login.php" >/dev/null; curl -sS -L -b /tmp/wpdev-cookies -c /tmp/wpdev-cookies --data-urlencode log=admin --data-urlencode pwd=integration-password --data-urlencode testcookie=1 "$1/wp-login.php" -o /tmp/wpdev-login; grep -q wordpress_logged_in /tmp/wpdev-cookies; curl -sS -L -b /tmp/wpdev-cookies "$1/wp-admin/" | grep -q wpadminbar`, "sh", strings.TrimSuffix(url, "/"))
					}
					if c.Web.Docroot == "." {
						code := dockerRun(t, c, "exec", "-T", "php", "curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", c.URL()+".wpdev.yml")
						if strings.TrimSpace(code) != "403" {
							t.Fatal("local artifacts must not be served", code)
						}
					}
					if err := startProject(c, false); err != nil {
						t.Fatal("native restart", err)
					}
					if tlsEnabled {
						wrong, _, _ := testCA(t)
						atomicWrite(c.file(".wpdev", "wrong-ca.pem"), wrong, 0o644)
						dockerRun(t, c, "exec", "-T", "php", "php", "-r", `$h=curl_init($argv[1]);curl_setopt_array($h,[CURLOPT_RETURNTRANSFER=>true,CURLOPT_CAINFO=>'/var/www/html/.wpdev/wrong-ca.pem',CURLOPT_TIMEOUT=>15]);if(curl_exec($h)!==false){exit(1);}`, c.URL())
						dockerRun(t, c, "exec", "-T", "php", "php", "-r", `$r=@file_get_contents($argv[1],false,stream_context_create(['ssl'=>['peer_name'=>'wrong.example.test'],'http'=>['timeout'=>15]]));if($r!==false){exit(1);}`, c.URL())
						dockerRun(t, c, "exec", "-T", "php", "curl", "-fsS", "--max-time", "30", "-o", "/dev/null", "https://wordpress.org/")
					}
					// Test both streaming utilities against the live MariaDB instance.
					backup, err := dumpDatabase(c)
					if err != nil {
						t.Fatal(err)
					}
					f, err := os.Open(backup)
					if err != nil {
						t.Fatal(err)
					}
					_, client := dbTools(c)
					err = compose(c, f, io.Discard, "exec", "-T", "db", client, "-uroot", "-proot", "wordpress")
					f.Close()
					if err != nil {
						t.Fatal(err)
					}
					dockerRun(t, c, "restart")
					for _, url := range urls {
						dockerRun(t, c, "exec", "-T", "php", "curl", "-fsS", "--retry", "10", "--retry-connrefused", "--retry-delay", "1", "--max-time", "20", "-o", "/dev/null", url)
					}
				})
			}
		}
	}
}

func TestDockerMySQL(t *testing.T) {
	dockerTests(t)
	c := dockerConfig(t, "apache", "8.5")
	c.Database.Engine = "mysql"
	c.Database.Version = "8.4"
	if err := renderTemplates(c); err != nil {
		t.Fatal(err)
	}
	dockerRun(t, c, "up", "-d", "--wait", "--wait-timeout", "180", "db")
	dockerRun(t, c, "exec", "-T", "db", "mysql", "-uroot", "-proot", "wordpress", "-e", "CREATE TABLE roundtrip (id INT); INSERT INTO roundtrip VALUES (42);")
	p, err := dumpDatabase(c)
	if err != nil {
		t.Fatal(err)
	}
	dockerRun(t, c, "exec", "-T", "db", "mysql", "-uroot", "-proot", "wordpress", "-e", "DROP TABLE roundtrip;")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = compose(c, f, io.Discard, "exec", "-T", "db", "mysql", "-uroot", "-proot", "wordpress"); err != nil {
		t.Fatal(err)
	}
	out := dockerRun(t, c, "exec", "-T", "db", "mysql", "-uroot", "-proot", "wordpress", "-N", "-e", "SELECT id FROM roundtrip;")
	if !strings.Contains(out, "42") {
		t.Fatal("restore failed", out)
	}
}

func TestDockerServices(t *testing.T) {
	dockerTests(t)
	c := dockerConfig(t, "nginx", "8.5")
	c.TLS.Enabled = true
	c.Services.Mailpit = true
	c.Services.Adminer = true
	if err := atomicWrite(c.file(c.Web.Docroot, "index.php"), []byte("<?php echo 'wpdev services';"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, ca, key := testCA(t)
	if err := atomicWrite(c.file(".wpdev", "certs", "rootCA.pem"), root, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ stem, host string }{{c.Domain, c.Domain}, {"_wildcard." + c.Domain, "*." + c.Domain}} {
		cert, priv := testLeaf(t, ca, key, item.host, time.Now().Add(90*24*time.Hour))
		if err := atomicWrite(c.file(".wpdev", "certs", item.stem+".pem"), cert, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := atomicWrite(c.file(".wpdev", "certs", item.stem+"-key.pem"), priv, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := tlsFingerprint(c); err != nil {
		t.Fatal(err)
	}
	if err := renderTemplates(c); err != nil {
		t.Fatal(err)
	}
	dockerRun(t, c, "up", "-d", "--build", "--wait", "--wait-timeout", "180")
	for _, host := range []string{c.Domain, "mail." + c.Domain, "db." + c.Domain} {
		dockerRun(t, c, "exec", "-T", "php", "curl", "-fsS", "-L", "--retry", "5", "--retry-delay", "1", "--max-time", "20", "-o", "/dev/null", "https://"+host)
	}
}
