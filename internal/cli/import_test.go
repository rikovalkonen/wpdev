package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func importFixture() (*Config, importMetadata) {
	c := defaultConfig()
	c.Name = "test"
	c.Domain = "masterportaali.test"
	c.TLS.Enabled = true
	m := importMetadata{Schema: "wordpress", Prefix: "prod_", Multisite: true, Mode: "subdirectory", Domain: "ope.nuortennyt.fi", BasePath: "/", NetworkID: 1, MainID: 1,
		Sites: []importSite{
			{1, "ope.nuortennyt.fi", "/", "https://ope.nuortennyt.fi", "https://ope.nuortennyt.fi"},
			{7, "ope.nuortennyt.fi", "/ohjeet/", "https://ope.nuortennyt.fi/ohjeet", "https://ope.nuortennyt.fi/ohjeet"},
		}}
	return c, m
}
func TestImportMappings(t *testing.T) {
	c, m := importFixture()
	p, err := makeImportPlan(c, m, importOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Target.Sites[1].Home != "https://masterportaali.test/ohjeet" || p.Target.Sites[1].ID != 7 {
		t.Fatal(p.Target)
	}
	if p.Replacements[0].Old != "https://ope.nuortennyt.fi/ohjeet" {
		t.Fatal("specific URLs must precede root", p.Replacements)
	}
	if c.Multisite.Enabled {
		t.Fatal("planning mutated live config")
	}
	m.Mode = "subdomain"
	m.Sites[1] = importSite{7, "ohjeet.ope.nuortennyt.fi", "/", "https://ohjeet.ope.nuortennyt.fi", "https://ohjeet.ope.nuortennyt.fi"}
	p, err = makeImportPlan(c, m, importOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Target.Sites[1].Domain != "ohjeet.masterportaali.test" {
		t.Fatal(p.Target)
	}
	c.Services.Mailpit = true
	m.Sites[1] = importSite{7, "mail.ope.nuortennyt.fi", "/", "https://mail.ope.nuortennyt.fi", "https://mail.ope.nuortennyt.fi"}
	if _, err = makeImportPlan(c, m, importOptions{}); err == nil {
		t.Fatal("service collision accepted")
	}
}
func TestImportMappedDomainsAndCollisions(t *testing.T) {
	c, m := importFixture()
	m.Sites[1] = importSite{7, "customer.example", "/", "https://customer.example", "https://customer.example"}
	if _, err := makeImportPlan(c, m, importOptions{}); err == nil {
		t.Fatal("external domain accepted without map")
	}
	file := filepath.Join(t.TempDir(), "map.yml")
	os.WriteFile(file, []byte("sites:\n  7: https://masterportaali.test/asiakas/\n"), 0o600)
	p, err := makeImportPlan(c, m, importOptions{Map: file})
	if err != nil {
		t.Fatal(err)
	}
	if p.Target.Sites[1].Path != "/asiakas/" {
		t.Fatal(p.Target)
	}
	for _, mapping := range []string{
		"sites:\n  7: https://masterportaali.test/\n",
		"sites:\n  7: https://elsewhere.test/\n",
		"sites:\n  7: http://masterportaali.test/asiakas/\n",
		"sites:\n  7: https://masterportaali.test/asiakas/\n  8: https://masterportaali.test/other/\n",
	} {
		os.WriteFile(file, []byte(mapping), 0o600)
		if _, err := makeImportPlan(c, m, importOptions{Map: file}); err == nil {
			t.Fatal("invalid mapping accepted", mapping)
		}
	}
}
func TestSQLGzipAndTruncation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "dump.sql.gz")
	var b bytes.Buffer
	g := gzip.NewWriter(&b)
	g.Write([]byte("CREATE TABLE test (id int);"))
	g.Close()
	os.WriteFile(file, b.Bytes(), 0o600)
	input, err := openSQL(file)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(input)
	input.Close()
	if err != nil || string(data) != "CREATE TABLE test (id int);" {
		t.Fatal(string(data), err)
	}
	os.WriteFile(file, b.Bytes()[:b.Len()-5], 0o600)
	input, err = openSQL(file)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err = io.ReadAll(input); err == nil {
		t.Fatal("truncated gzip accepted")
	}
}
func TestRemoteCommandsAndFailedExport(t *testing.T) {
	c, _ := importFixture()
	c.Root = t.TempDir()
	remote := SSHCfg{Host: "production", User: "deploy", Port: 2222, WPPath: "/srv/a'b $(touch nope)"}
	old := executeProcess
	defer func() { executeProcess = old }()
	var calls [][]string
	executeProcess = func(cmd *exec.Cmd) error {
		calls = append(calls, cmd.Args)
		last := cmd.Args[len(cmd.Args)-1]
		switch {
		case strings.Contains(last, "'eval'"):
			io.WriteString(cmd.Stdout, `{"prefix":"wp_","mode":"subdirectory","uploads":"/srv/uploads"}`)
		case strings.Contains(last, "'tables'"):
			io.WriteString(cmd.Stdout, "wp_options,wp_posts,wp_users")
		default:
			io.WriteString(cmd.Stdout, "partial SQL")
			return errors.New("SSH interrupted")
		}
		return nil
	}
	dir := t.TempDir()
	_, _, err := fetchRemote(c, remote, dir)
	if err == nil {
		t.Fatal("failed export accepted")
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatal("partial export was published", files)
	}
	if len(calls) != 3 {
		t.Fatal(calls)
	}
	command := calls[0][len(calls[0])-1]
	if !strings.Contains(command, `'--path=/srv/a'\''b $(touch nope)'`) {
		t.Fatal("path not shell quoted", command)
	}
	if !strings.Contains(strings.Join(calls[0], " "), "-p 2222 -- deploy@production") {
		t.Fatal(calls[0])
	}
	for _, host := range []string{"-oProxyCommand=bad", "host;bad", "user@host", "host\nbad"} {
		remote.Host = host
		if err = remote.validate(); err == nil {
			t.Fatal("unsafe host accepted", host)
		}
	}
}
func TestImportSnapshotRestoresFilesAndRemovesAdditions(t *testing.T) {
	c, _ := importFixture()
	c.Root = t.TempDir()
	c.ConfigPath = c.file(".wpdev.yml")
	files := map[string]string{".wpdev.yml": "old config", "wp/wp-config.php": "secret", ".wpdev/generated/file": "old", ".wpdev/templates/custom": "custom"}
	for path, b := range files {
		if err := atomicWrite(c.file(path), []byte(b), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	s, err := snapshotImport(c)
	if err != nil {
		t.Fatal(err)
	}
	for path := range files {
		atomicWrite(c.file(path), []byte("changed"), 0o644)
	}
	atomicWrite(c.file(".wpdev", "generated", "new"), []byte("new"), 0o644)
	if err = s.restore(); err != nil {
		t.Fatal(err)
	}
	for path, want := range files {
		b, err := os.ReadFile(c.file(path))
		if err != nil || string(b) != want {
			t.Fatal(path, string(b), err)
		}
		info, _ := os.Stat(c.file(path))
		if info.Mode().Perm() != 0o640 {
			t.Fatal("permissions lost", path)
		}
	}
	if _, err = os.Stat(c.file(".wpdev", "generated", "new")); !os.IsNotExist(err) {
		t.Fatal("new generated file survived restore")
	}
}
func TestImportConfigValues(t *testing.T) {
	c, m := importFixture()
	p, err := makeImportPlan(c, m, importOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v := p.configValues()
	if v["WP_HOME"] != nil || v["DOMAIN_CURRENT_SITE"] != "masterportaali.test" || v["table_prefix"] != "prod_" {
		t.Fatal(v)
	}
}
func TestSingleSiteCoreURLMovesToConfiguredDocumentRoot(t *testing.T) {
	c, m := importFixture()
	m.Multisite = false
	m.Mode = ""
	m.BasePath = "/blog/"
	m.Sites = []importSite{{1, m.Domain, "/blog/", "https://ope.nuortennyt.fi/blog", "https://ope.nuortennyt.fi/blog/wp"}}
	p, err := makeImportPlan(c, m, importOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Target.Sites[0].Home != "https://masterportaali.test" || p.Target.Sites[0].SiteURL != "https://masterportaali.test" {
		t.Fatal(p.Target)
	}
	if p.Replacements[0].Old != "https://ope.nuortennyt.fi/blog/wp" {
		t.Fatal(p.Replacements)
	}
}

func TestLocalRuntimeRejectsProductionOverrides(t *testing.T) {
	c, _ := importFixture()
	c.Root = t.TempDir()
	old := executeProcess
	defer func() { executeProcess = old }()
	for _, env := range []map[string]string{
		{}, {"WPDEV_LOCAL": "0"}, {"WPDEV_LOCAL": "1", "WPDEV_DB_HOST": "production.example"},
	} {
		executeProcess = func(cmd *exec.Cmd) error {
			return json.NewEncoder(cmd.Stdout).Encode(map[string]any{"services": map[string]any{"php": map[string]any{"environment": env}}})
		}
		if err := validateLocalRuntime(c); err == nil {
			t.Fatal("unsafe PHP environment accepted", env)
		}
	}
	executeProcess = func(cmd *exec.Cmd) error {
		io.WriteString(cmd.Stdout, `{"services":{"php":{"environment":{"WPDEV_LOCAL":"1"}}}}`)
		return nil
	}
	if err := validateLocalRuntime(c); err != nil {
		t.Fatal(err)
	}
}

func TestUploadsRsync(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync is not installed")
	}
	c, _ := importFixture()
	c.Root = t.TempDir()
	source := filepath.Join(t.TempDir(), "uploads with 'quote")
	os.MkdirAll(source, 0o755)
	os.WriteFile(filepath.Join(source, "image.txt"), []byte("remote image"), 0o644)
	dest := c.file("wp", "wp-content", "uploads")
	atomicWrite(filepath.Join(dest, "image.txt"), []byte("original image"), 0o644)
	atomicWrite(filepath.Join(dest, "local-only.txt"), []byte("keep me"), 0o644)
	bin := t.TempDir()
	// Simulate only the SSH transport; use the real rsync sender and receiver
	// against local test directories, including a shell-sensitive remote path.
	ssh := `#!/bin/sh
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
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := executeProcess
	defer func() { executeProcess = old }()
	executeProcess = func(cmd *exec.Cmd) error {
		if filepath.Base(cmd.Path) == "docker" {
			io.WriteString(cmd.Stdout, "/var/www/html/wp/wp-content/uploads")
			return nil
		}
		if filepath.Base(cmd.Path) == "rsync" {
			for _, arg := range cmd.Args {
				if arg == "--delete" {
					t.Fatal("uploads must never delete local extras")
				}
			}
		}
		return cmd.Run()
	}
	if err := syncUploads(c, SSHCfg{Host: "fixture", WPPath: "/wordpress"}, source, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dest, "image.txt"))
	if string(b) != "remote image" {
		t.Fatal("image not copied", string(b))
	}
	if _, err := os.Stat(filepath.Join(dest, "local-only.txt")); err != nil {
		t.Fatal("local extra removed")
	}
	backups, _ := filepath.Glob(c.file(".wpdev", "backups", "uploads-*", "image.txt"))
	if len(backups) != 1 {
		t.Fatal("overwritten image not backed up", backups)
	}
	b, _ = os.ReadFile(backups[0])
	if string(b) != "original image" {
		t.Fatal(string(b))
	}
}

func TestDockerConfigLocal(t *testing.T) {
	dockerTests(t)
	c, _ := importFixture()
	c.Root = t.TempDir()
	source := `<?php
declare(strict_types=1);
// retain comments and a nontrivial production expression
define('DB_NAME', getenv('PROD_DB') ?: 'production');
define('DB_USER', 'production-user');
define('DB_PASSWORD', str_replace('x', ',', 'sxcret'));
define('DB_HOST', (getenv('PROD_HOST') ?: 'production-db'));
$table_prefix = 'prod_';
define('SUBDOMAIN_INSTALL', false);
define('WP_HOME', 'https://production.example');
define('WP_SITEURL', 'https://production.example');
define('COOKIE_DOMAIN', '.production.example');
`
	values := localDBValues()
	values["WP_HOME"] = nil
	values["WP_SITEURL"] = nil
	values["COOKIE_DOMAIN"] = nil
	values["table_prefix"] = "new_"
	values["MULTISITE"] = true
	r, err := transformConfig(c, []byte(source), values)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := transformConfig(c, []byte(r.Source), values)
	if err != nil || r.Source != r2.Source {
		t.Fatal("not idempotent", err)
	}
	if r.Hints.Mode != "subdirectory" {
		t.Fatal(r.Hints)
	}
	run := func(local bool) map[string]any {
		code := r.Source + ` echo json_encode([DB_NAME, DB_HOST, DB_PASSWORD, $table_prefix, defined('WP_HOME'), defined('MULTISITE')]);`
		cmd := exec.Command("docker", "run", "--rm", "-i", "--network=none", "-e", fmtEnv(local), "php:8.5-cli", "php")
		cmd.Stdin = strings.NewReader(code)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("evaluate transformed config: %v %s", err, out)
		}
		var data []any
		if err = json.Unmarshal(out, &data); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return map[string]any{"db": data[0], "host": data[1], "password": data[2], "prefix": data[3], "home": data[4], "multi": data[5]}
	}
	prod := run(false)
	local := run(true)
	if prod["db"] != "production" || prod["password"] != "s,cret" || prod["home"] != true || prod["multi"] != false {
		t.Fatal(prod)
	}
	if local["db"] != "wordpress" || local["host"] != "db" || local["prefix"] != "new_" || local["home"] != false || local["multi"] != true {
		t.Fatal(local)
	}
	for _, bad := range []string{
		source + "\ndefine('DB_NAME', 'duplicate');",
		strings.Replace(source, "define('DB_NAME', getenv('PROD_DB') ?: 'production');", "require 'private-db.php';", 1),
	} {
		if _, err = transformConfig(c, []byte(bad), values); err == nil {
			t.Fatal("ambiguous config accepted")
		}
	}
}
func fmtEnv(local bool) string {
	if local {
		return "WPDEV_LOCAL=1"
	}
	return "WPDEV_LOCAL=0"
}

func importTestCommand() *cobra.Command {
	c := &cobra.Command{}
	c.SetIn(strings.NewReader("yes\n"))
	return c
}
