package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func localRsyncSSH(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync is not installed")
	}
	bin := t.TempDir()
	script := `#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
  case "$1" in
    -T) shift ;;
    -o|-p|-l) shift 2 ;;
    *) shift; break ;;
  esac
done
exec /bin/sh -c "$*"
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writePullFixture(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"wp-load.php", "wp-settings.php", "wp-includes/version.php", "wp-config.php", "wp-content/plugins/example/main.php", "wp-content/plugins/example/install.sql", "wp-content/themes/example/style.css", "wp-content/themes/example/ääkköset @2x.txt", "site.txt"} {
		if err := atomicWrite(filepath.Join(root, name), []byte("remote "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPullFileTransferAndRollback(t *testing.T) {
	localRsyncSSH(t)
	for _, docroot := range []string{".", "wp"} {
		t.Run(docroot, func(t *testing.T) {
			c := testConfig(t)
			c.Web.Docroot = docroot
			c.ConfigPath = c.file("custom.yml")
			c.Database.Persist = "bind"
			c.Database.DataPath = filepath.Join(docroot, "database")
			c.Pull.Exclude = []string{"/wp-content/themes/private"}
			source := filepath.Join(t.TempDir(), "source with 'quote")
			writePullFixture(t, source)
			excluded := []string{".git/config", ".wpdev/generated/secret", ".env", ".htaccess", ".user.ini", "wp-cli.local.yml", "dump.sql", "wp-content/object-cache.php", "wp-content/uploads/image.jpg", "wp-content/cache/file", "wp-content/mu-plugins/wpdev-local.php", "wp-content/themes/private/style.css", "wp-content/plugins/managed/main.php", "database/ibdata1"}
			if docroot == "." {
				excluded = append(excluded, "custom.yml")
			}
			for _, name := range excluded {
				atomicWrite(filepath.Join(source, name), []byte("never copy"), 0o644)
			}
			writePluginState(c, pluginState{Plugins: map[string]pluginRecord{"managed": {pluginSpec: pluginSpec{Source: "managed"}, File: "managed/main.php", Managed: true, Enabled: true}}})
			atomicWrite(c.file(docroot, "site.txt"), []byte("original"), 0o640)
			atomicWrite(c.file(docroot, "only-local.txt"), []byte("keep"), 0o644)
			atomicWrite(c.file("custom.yml"), []byte("local configuration"), 0o600)
			old := executeProcess
			fakeProcesses(t, func(cmd *exec.Cmd) error {
				if filepath.Base(cmd.Path) == "ssh" {
					if !strings.Contains(cmd.Args[len(cmd.Args)-1], "'config' 'path'") {
						t.Fatal(cmd.Args)
					}
					fmt.Fprintln(cmd.Stdout, filepath.Join(source, "wp-config.php"))
					return nil
				}
				return old(cmd)
			})
			meta := remoteMetadata{Root: source, Content: filepath.Join(source, "wp-content"), Plugins: filepath.Join(source, "wp-content/plugins"), Uploads: filepath.Join(source, "wp-content/uploads")}
			stage := filepath.Join(t.TempDir(), "stage")
			var preview bytes.Buffer
			files, err := fetchRemoteFiles(c, SSHCfg{Host: "fixture", Port: 2222, WPPath: source}, meta, stage, &preview)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range excluded {
				if _, err := os.Lstat(filepath.Join(stage, name)); !os.IsNotExist(err) {
					t.Fatal("excluded file downloaded", name, err)
				}
			}
			if _, err := os.Stat(filepath.Join(stage, "wp-content/plugins/example/install.sql")); err != nil {
				t.Fatal("plugin SQL source was mistaken for a backup", err)
			}
			if b, _ := os.ReadFile(c.file(docroot, "site.txt")); string(b) != "original" {
				t.Fatal("download changed project")
			}
			if !strings.Contains(preview.String(), "1 replacements") {
				t.Fatal(preview.String())
			}
			if err = files.check(c); err != nil {
				t.Fatal(err)
			}
			files.newConfig = true
			snapshot, err := snapshotManaged(c, "import", files.snapshotPaths(c)...)
			if err != nil {
				t.Fatal(err)
			}
			if err = files.publish(c); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(c.file(".gitignore")); !strings.Contains(string(b), "/"+filepath.ToSlash(filepath.Join(docroot, "wp-config.php"))+"\n") {
				t.Fatal("imported config was not ignored", string(b))
			}
			if b, _ := os.ReadFile(c.file(docroot, "site.txt")); string(b) != "remote site.txt" {
				t.Fatal(string(b))
			}
			if err = snapshot.restore(); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(c.file(docroot, "site.txt")); string(b) != "original" {
				t.Fatal("rollback did not restore overwritten file")
			}
			if _, err := os.Stat(c.file(docroot, "wp-includes")); !os.IsNotExist(err) {
				t.Fatal("rollback left new directories")
			}
			if b, _ := os.ReadFile(c.file(docroot, "only-local.txt")); string(b) != "keep" {
				t.Fatal("local extra removed")
			}
		})
	}
}

func TestPullFileGuards(t *testing.T) {
	c := testConfig(t)
	for _, name := range []string{".GIT/config", ".WPDEV/generated/file", "docker-compose.yml", filepath.Base(c.ConfigPath)} {
		if err := checkPullPath(c, name); err == nil {
			t.Fatal("local infrastructure path accepted", name)
		}
	}
	source := t.TempDir()
	writePullFixture(t, source)
	files, err := planPullFiles(c, source, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	atomicWrite(c.file(c.Web.Docroot, "site.txt"), []byte("editor changed this"), 0o644)
	if err = files.check(c); err == nil || !strings.Contains(err.Error(), "changed during pull") {
		t.Fatal("concurrent edit accepted", err)
	}
	if err = os.Symlink(t.TempDir(), c.file(c.Web.Docroot, "wp-content")); err != nil {
		t.Fatal(err)
	}
	if _, err = planPullFiles(c, source, io.Discard); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatal("local symlink accepted", err)
	}
	os.Remove(c.file(c.Web.Docroot, "wp-content"))
	os.Symlink("site.txt", filepath.Join(source, "link.txt"))
	if _, err = planPullFiles(c, source, io.Discard); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatal("remote symlink accepted", err)
	}
}

func TestPullFileInterruptedDownload(t *testing.T) {
	c := testConfig(t)
	atomicWrite(c.file(c.Web.Docroot, "site.txt"), []byte("original"), 0o644)
	stage := filepath.Join(t.TempDir(), "stage")
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		if filepath.Base(cmd.Path) != "rsync" {
			t.Fatal("continued after failed transfer", cmd.Args)
		}
		atomicWrite(filepath.Join(stage, "site.txt"), []byte("partial download"), 0o644)
		return fmt.Errorf("interrupted rsync")
	})
	meta := remoteMetadata{Root: "/wordpress", Content: "/wordpress/wp-content", Plugins: "/wordpress/wp-content/plugins"}
	if _, err := fetchRemoteFiles(c, SSHCfg{Host: "fixture", WPPath: "/wordpress"}, meta, stage, io.Discard); err == nil {
		t.Fatal("failed download accepted")
	}
	if b, _ := os.ReadFile(c.file(c.Web.Docroot, "site.txt")); string(b) != "original" {
		t.Fatal("failed download changed project")
	}
}

func TestPullDefaultsAndDBOnly(t *testing.T) {
	c := testConfig(t)
	c.SSH = map[string]SSHCfg{"production": {Host: "fixture", WPPath: "/wordpress"}}
	if err := saveConfig(c.ConfigPath, c); err != nil {
		t.Fatal(err)
	}
	before := cfgFile
	cfgFile = c.ConfigPath
	t.Cleanup(func() { cfgFile = before; pullCmd.Flags().Set("db-only", "false") })
	transfers := 0
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		if filepath.Base(cmd.Path) == "rsync" {
			transfers++
			return fmt.Errorf("test transfer stop")
		}
		last := cmd.Args[len(cmd.Args)-1]
		switch {
		case strings.Contains(last, "'eval'"):
			io.WriteString(cmd.Stdout, `{"prefix":"wp_","root":"/wordpress","content":"/wordpress/wp-content","plugins":"/wordpress/wp-content/plugins"}`)
		case strings.Contains(last, "'tables'"):
			io.WriteString(cmd.Stdout, "wp_options,wp_posts")
		case strings.Contains(last, "'export'"):
			io.WriteString(cmd.Stdout, "SQL fixture")
		default:
			t.Fatal(cmd.Args)
		}
		return nil
	})
	if err := pullCmd.RunE(pullCmd, []string{"production"}); err == nil || transfers != 1 {
		t.Fatal("default pull did not fetch files", err, transfers)
	}
	pullCmd.Flags().Set("db-only", "true")
	if err := pullCmd.RunE(pullCmd, []string{"production"}); err == nil || transfers != 1 {
		t.Fatal("db-only fetched files or accepted missing local WordPress", err, transfers)
	}
}
