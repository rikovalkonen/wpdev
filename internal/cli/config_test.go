package cli

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	c := defaultConfig()
	c.Name = "example"
	c.Domain = "example.test"
	c.Root = t.TempDir()
	c.ConfigPath = c.file(".wpdev.yml")
	return c
}
func fakeProcesses(t *testing.T, f func(*exec.Cmd) error) {
	t.Helper()
	old := executeProcess
	executeProcess = f
	t.Cleanup(func() { executeProcess = old })
}
func TestConfigPathsAndPreservation(t *testing.T) {
	c := testConfig(t)
	p := c.file("settings.yml")
	input := `# project note
name: sample
domain: sample.test
web:
  server: apache
  php: "8.3" # deployment version
  docroot: wp
database:
  engine: mysql
ssh:
  production:
    host: example.org
xdebug:
  enabled: false
`
	if err := os.WriteFile(p, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Root != c.Root || loaded.Database.Version != "8.4" || loaded.Database.Persist != "volume" {
		t.Fatalf("bad defaults/root: %+v", loaded)
	}
	loaded.Xdebug.Enabled = true
	if err = saveConfig(p, loaded); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, want := range []string{"# project note", "# deployment version", "host: example.org", "enabled: true", "8.3"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("lost %q in %s", want, b)
		}
	}
	old := cfgFile
	cfgFile = p
	t.Cleanup(func() { cfgFile = old })
	actual, err := projectConfig()
	if err != nil || actual.ConfigPath != p {
		t.Fatalf("--config ignored: %v", err)
	}
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		if cmd.Dir != c.Root || !strings.Contains(strings.Join(cmd.Args, " "), p[:len(p)-len("settings.yml")]+"docker-compose.yml") {
			t.Fatalf("wrong compose root: %v / %s", cmd.Args, cmd.Dir)
		}
		return nil
	})
	if err = compose(actual, nil, io.Discard, "down"); err != nil {
		t.Fatal(err)
	}
}
func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"domain URL", func(c *Config) { c.Domain = "https://example.test" }},
		{"shell path", func(c *Config) { c.Web.Docroot = "$(touch bad)" }},
		{"parent path", func(c *Config) { c.Web.Docroot = "../outside" }},
		{"server", func(c *Config) { c.Web.Server = "invalid" }},
		{"port", func(c *Config) { c.Database.Portforward = "65536" }},
		{"mode", func(c *Config) { c.Multisite.Mode = "wildcard" }},
		{"base", func(c *Config) { c.Multisite.BasePath = "/bad/../" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			tc.change(c)
			if c.validate() == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing.yml")); err == nil {
		t.Fatal("missing config accepted")
	}
}
func TestPromptReadsAllLinesAndEOF(t *testing.T) {
	p := &prompter{reader: bufio.NewReader(strings.NewReader("first\nsecond\n\n")), out: io.Discard}
	if p.ask("one", "") != "first" || p.ask("two", "") != "second" || p.ask("three", "default") != "default" || p.err != nil {
		t.Fatal("buffered input lost")
	}
	p.ask("four", "")
	if p.err == nil {
		t.Fatal("EOF must not silently choose defaults")
	}
}
func TestInitKeepsExistingConfiguration(t *testing.T) {
	c := testConfig(t)
	c.Web.Docroot = "custom"
	c.Web.PHP = "8.3"
	if err := saveConfig(c.ConfigPath, c); err != nil {
		t.Fatal(err)
	}
	old, oldDefaults := cfgFile, initDefaults
	cfgFile = c.ConfigPath
	initDefaults = false
	t.Cleanup(func() { cfgFile = old; initDefaults = oldDefaults; initCmd.SetIn(nil); initCmd.SetOut(nil) })
	initCmd.SetIn(strings.NewReader("n\n"))
	initCmd.SetOut(io.Discard)
	if err := initCmd.RunE(initCmd, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.file("custom", "index.php")); err != nil {
		t.Fatal("existing docroot not used", err)
	}
	after, err := loadConfig(c.ConfigPath)
	if err != nil || after.Web.PHP != "8.3" {
		t.Fatal("existing config changed", err)
	}
}
func TestDatabaseStreamingAndErrors(t *testing.T) {
	for _, engine := range []string{"mariadb", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			c := testConfig(t)
			c.Database.Engine = engine
			dump, _ := dbTools(c)
			fakeProcesses(t, func(cmd *exec.Cmd) error {
				if !strings.Contains(strings.Join(cmd.Args, " "), dump) {
					t.Fatal(cmd.Args)
				}
				_, err := io.WriteString(cmd.Stdout, "CREATE TABLE test (id INT);\n")
				return err
			})
			p, err := dumpDatabase(c)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(p)
			if !bytes.Contains(b, []byte("CREATE TABLE")) {
				t.Fatal("empty backup")
			}
		})
	}
	c := testConfig(t)
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		io.WriteString(cmd.Stdout, "partial")
		io.WriteString(cmd.Stderr, "server unavailable")
		return errors.New("exit 1")
	})
	if _, err := dumpDatabase(c); err == nil || !strings.Contains(err.Error(), "server unavailable") {
		t.Fatal("lost process error", err)
	}
	entries, _ := os.ReadDir(c.file(".wpdev", "db"))
	if len(entries) != 0 {
		t.Fatal("published partial backup")
	}
}
