package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerImportDiagnostics(t *testing.T) {
	dockerTests(t)
	c := dockerConfig(t, "nginx", "8.5")
	c.Domain = "production.example"
	if err := os.MkdirAll(c.file(c.Web.Docroot), 0o755); err != nil {
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
	dockerRun(t, c, wpArgs(c, "config", "set", "WP_DEBUG", "true", "--raw")...)
	dockerRun(t, c, wpArgs(c, "config", "set", "WP_DEBUG_DISPLAY", "false", "--raw")...)
	dockerRun(t, c, wpArgs(c, "core", "install", "--url="+c.URL(), "--title=Diagnostics fixture", "--admin_user=admin", "--admin_password=fixture-password", "--admin_email=admin@example.test", "--skip-email")...)
	dockerRun(t, c, wpArgs(c, "eval", `update_option('diagnostic_object', (object)['url'=>home_url('/article')]); class WPDev_Unloaded_Fixture { public $url='https://unrelated.example'; } update_option('diagnostic_unknown', new WPDev_Unloaded_Fixture);`)...)
	dump, err := dumpDatabase(c)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := configSource(c)
	if err != nil {
		t.Fatal(err)
	}
	transformed, err := transformConfig(c, source, localDBValues())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := newImportStage(c, []byte(transformed.Source))
	if err != nil {
		t.Fatal(err)
	}
	defer stage.close()
	if err = stage.importSQL(dump); err != nil {
		t.Fatal(err)
	}
	meta, err := stage.inspect(importOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	target := *c
	target.Domain = "localized.test"
	plan, err := makeImportPlan(&target, meta, importOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	original := executeProcess
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		if cmd.Dir == stage.dir && strings.Contains(strings.Join(cmd.Args, " "), "/usr/local/bin/wp") {
			cmd.Stderr = io.MultiWriter(cmd.Stderr, &diagnostics)
		}
		return original(cmd)
	})
	if _, err = stage.localize(plan, source); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diagnostics.String(), "Deprecated:") || strings.Contains(diagnostics.String(), "wp_is_block_theme") {
		t.Fatalf("import emitted compatibility noise: %s", &diagnostics)
	}
	if !strings.Contains(diagnostics.String(), `Skipping an uninitialized class "WPDev_Unloaded_Fixture"`) {
		t.Fatal("meaningful serialization warning was lost", &diagnostics)
	}
	var out bytes.Buffer
	if err = stage.wp(&out, "eval", `echo get_option('diagnostic_object')->url;`); err != nil {
		t.Fatal(err)
	}
	if expected := target.URL() + "article"; strings.TrimSpace(out.String()) != expected {
		t.Fatalf("serialized object: got %q, want %q", out.String(), expected)
	}
	// Suppressing compatibility chatter must not turn genuine failures into
	// successful imports, even when the source config enables WordPress debug.
	err = stage.wp(io.Discard, "eval", `trigger_error('wpdev diagnostic warning', E_USER_WARNING); throw new RuntimeException('wpdev diagnostic failure');`)
	if err == nil || !strings.Contains(err.Error(), "wpdev diagnostic failure") || !strings.Contains(diagnostics.String(), "wpdev diagnostic warning") {
		t.Fatal("warning or failure was hidden", err, &diagnostics)
	}
	if _, err = os.Stat(filepath.Join(stage.dir, "content", "themes")); err != nil {
		t.Fatal(err)
	}
}
