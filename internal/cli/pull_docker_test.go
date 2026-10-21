package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only SSH transport is simulated: source WP-CLI, SQL export, rsync, staging,
// publication and rollback all execute against real isolated fixtures.
func pullDockerTransport(t *testing.T, source *Config) {
	t.Helper()
	localRsyncSSH(t)
	original := executeProcess
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		switch filepath.Base(cmd.Path) {
		case "ssh":
			command := strings.ReplaceAll(cmd.Args[len(cmd.Args)-1], shellQuote("--path="+source.file(source.Web.Docroot)), shellQuote("--path="+source.containerDocroot()))
			if source.Database.Engine == "mysql" && strings.Contains(command, "'db' 'export'") {
				// The test PHP image ships MariaDB's dump client, which distrusts
				// MySQL's generated test certificate. Only this isolated fixture
				// uses plaintext; real SSH exports retain their server's TLS policy.
				command += " '--skip-ssl'"
			}
			proxy := exec.Command("docker", composeArgs(source, "exec", "-T", "php", "sh", "-c", command)...)
			proxy.Dir, proxy.Stdin, proxy.Stdout, proxy.Stderr = source.Root, cmd.Stdin, cmd.Stdout, cmd.Stderr
			return original(proxy)
		case "rsync":
			for i, arg := range cmd.Args {
				if strings.HasPrefix(arg, "fixture:") {
					cmd.Args[i] = strings.ReplaceAll(arg, source.containerDocroot(), source.file(source.Web.Docroot))
				}
			}
		}
		return original(cmd)
	})
}

func TestDockerPullFiles(t *testing.T) {
	dockerTests(t)
	for _, tc := range []struct{ mode, server, engine, docroot string }{
		{"single", "apache", "mariadb", "."},
		{"subdirectory", "nginx", "mysql", "wp"},
		{"subdomain", "nginx", "mariadb", "wp"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			source := dockerConfig(t, "apache", "8.5")
			source.Domain = "production.example"
			source.Database.Engine = tc.engine
			if tc.engine == "mysql" {
				source.Database.Version = "8.4"
			}
			os.MkdirAll(source.file(source.Web.Docroot), 0o755)
			if err := renderTemplates(source); err != nil {
				t.Fatal(err)
			}
			dockerRun(t, source, "up", "-d", "--build", "--wait", "--wait-timeout", "180")
			version := os.Getenv("WPDEV_WP_VERSION")
			if version == "" {
				version = "latest"
			}
			dockerRun(t, source, wpArgs(source, "core", "download", "--force", "--version="+version)...)
			dockerRun(t, source, wpArgs(source, "config", "create", "--dbname=wordpress", "--dbuser=wp", "--dbpass=secret", "--dbhost=db", "--skip-check")...)
			args := []string{"core", "install"}
			if tc.mode != "single" {
				args = []string{"core", "multisite-install", "--base=/"}
				if tc.mode == "subdomain" {
					args = append(args, "--subdomains")
				}
			}
			args = append(args, "--url="+source.URL(), "--title=Pull fixture", "--admin_user=admin", "--admin_password=pull-test-password", "--admin_email=admin@example.test", "--skip-email")
			dockerRun(t, source, wpArgs(source, args...)...)
			if tc.mode != "single" {
				dockerRun(t, source, wpArgs(source, "site", "create", "--slug=child", "--title=Child", "--email=admin@example.test")...)
			}
			plugin := "wp-content/plugins/pull-fixture/main.php"
			pluginCode := "<?php\n/* Plugin Name: Pull fixture */\ndefine('WPDEV_PULL_FIXTURE', 'source-one');\n"
			atomicWrite(source.file(source.Web.Docroot, plugin), []byte(pluginCode), 0o644)
			activation := []string{"plugin", "activate", "pull-fixture"}
			if tc.mode != "single" {
				activation = append(activation, "--network")
			}
			dockerRun(t, source, wpArgs(source, activation...)...)
			dockerRun(t, source, wpArgs(source, "option", "update", "pull_marker", "source-one")...)
			for name, content := range map[string]string{"pulled.txt": "source-one", "wp-content/themes/pull-fixture/style.css": "/* Theme Name: Pull fixture */", "wp-content/uploads/source.txt": "media", ".htaccess": "SetEnv WP_HOME https://production.example\n", "wp-content/cache/server-cache": "skip cache"} {
				atomicWrite(source.file(source.Web.Docroot, name), []byte(content), 0o644)
			}
			c := dockerConfig(t, tc.server, "8.5")
			c.Domain = "pulled.test"
			c.Web.Docroot = tc.docroot
			c.Database.Engine, c.Database.Version = source.Database.Engine, source.Database.Version
			c.SSH = map[string]SSHCfg{"production": {Host: "fixture", WPPath: source.file(source.Web.Docroot)}}
			c.TLS.Enabled = tc.mode == "subdomain"
			if c.TLS.Enabled {
				t.Setenv("CAROOT", c.file(".wpdev", "certs"))
				root, ca, key := testCA(t)
				atomicWrite(c.file(".wpdev", "certs", "rootCA.pem"), root, 0o644)
				for _, item := range []struct{ stem, host string }{{c.Domain, c.Domain}, {"_wildcard." + c.Domain, "*." + c.Domain}} {
					cert, priv := testLeaf(t, ca, key, item.host, time.Now().Add(90*24*time.Hour))
					atomicWrite(c.file(".wpdev", "certs", item.stem+".pem"), cert, 0o644)
					atomicWrite(c.file(".wpdev", "certs", item.stem+"-key.pem"), priv, 0o600)
				}
			}
			if err := saveConfig(c.ConfigPath, c); err != nil {
				t.Fatal(err)
			}
			pullDockerTransport(t, source)
			oldConfig, oldOut := cfgFile, pullCmd.OutOrStdout()
			cfgFile = c.ConfigPath
			pullCmd.SetOut(io.Discard)
			pullCmd.Flags().Set("yes", "true")
			t.Cleanup(func() {
				cfgFile = oldConfig
				pullCmd.SetOut(oldOut)
				for _, flag := range []string{"yes", "dry-run", "uploads", "uploads-only"} {
					pullCmd.Flags().Set(flag, "false")
				}
			})
			if tc.mode == "single" {
				pullCmd.Flags().Set("dry-run", "true")
				if err := pullCmd.RunE(pullCmd, []string{"production"}); err != nil {
					t.Fatal("empty project preview", err)
				}
				for _, name := range []string{"wp-load.php", "wp-config.php", "pulled.txt", "docker-compose.yml"} {
					if _, err := os.Stat(c.file(name)); !os.IsNotExist(err) {
						t.Fatal("preview changed project", name)
					}
				}
				pullCmd.Flags().Set("dry-run", "false")
			}
			if err := pullCmd.RunE(pullCmd, []string{"production"}); err != nil {
				t.Fatal("pull into empty project", err)
			}
			loaded, err := loadConfig(c.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			*c = *loaded
			check := `if(WPDEV_PULL_FIXTURE!=='source-one'||get_option('pull_marker')!=='source-one'||DB_HOST!=='db')throw new RuntimeException('files or DB missing');echo 'pull-ok';`
			dockerRun(t, c, wpArgs(c, "eval", check)...)
			if c.Multisite.Enabled != (tc.mode != "single") {
				t.Fatal("network type was not imported")
			}
			for _, name := range []string{"wp-load.php", "wp-includes/version.php", "wp-content/themes/pull-fixture/style.css", "pulled.txt"} {
				if _, err := os.Stat(c.file(c.Web.Docroot, name)); err != nil {
					t.Fatal("source file missing", name, err)
				}
			}
			if _, err := os.Stat(c.file(c.Web.Docroot, "wp-content/uploads/source.txt")); !os.IsNotExist(err) {
				t.Fatal("uploads copied without flag")
			}
			if body := dockerRun(t, c, "exec", "-T", "php", "curl", "--fail", "-sS", c.URL()+"pulled.txt"); body != "source-one" {
				t.Fatal(body)
			}
			if tc.mode == "single" {
				atomicWrite(c.file(c.Web.Docroot, "only-local.txt"), []byte("keep"), 0o644)
				dockerRun(t, source, wpArgs(source, "option", "update", "pull_marker", "source-two")...)
				atomicWrite(source.file(source.Web.Docroot, "pulled.txt"), []byte("source-two"), 0o644)
				atomicWrite(source.file(source.Web.Docroot, plugin), []byte("<?php\n/* Plugin Name: Pull fixture */\nthrow new RuntimeException('production plugin fails locally');"), 0o644)
				if err := pullCmd.RunE(pullCmd, []string{"production"}); err == nil || !strings.Contains(err.Error(), "restored") {
					t.Fatal("failed full pull was not restored", err)
				}
				dockerRun(t, c, wpArgs(c, "eval", check)...)
				if b, _ := os.ReadFile(c.file(c.Web.Docroot, "pulled.txt")); string(b) != "source-one" {
					t.Fatal("file rollback failed")
				}
				atomicWrite(source.file(source.Web.Docroot, plugin), []byte(pluginCode), 0o644)
				pullCmd.Flags().Set("uploads", "true")
				if err := pullCmd.RunE(pullCmd, []string{"production"}); err != nil {
					t.Fatal("retry full pull with uploads", err)
				}
				for name, want := range map[string]string{"only-local.txt": "keep", "pulled.txt": "source-two", "wp-content/uploads/source.txt": "media"} {
					if b, _ := os.ReadFile(c.file(c.Web.Docroot, name)); string(b) != want {
						t.Fatalf("%s: %s != %s", name, b, want)
					}
				}
				dockerRun(t, c, wpArgs(c, "eval", `if(get_option('pull_marker')!=='source-two')throw new RuntimeException('retry failed');`)...)
			}
			fmt.Fprintln(os.Stdout, "Full pull fixture passed:", tc.mode)
		})
	}
}
