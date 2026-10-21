package cli

import (
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRenderMatrix(t *testing.T) {
	fakeProcesses(t, func(cmd *exec.Cmd) error { return nil })
	for _, php := range []string{"8.3", "8.4", "8.5"} {
		for _, server := range []string{"apache", "nginx"} {
			for _, mode := range []string{"single", "subdirectory", "subdomain"} {
				for _, tls := range []bool{false, true} {
					for _, docroot := range []string{".", "wp"} {
						name := php + "/" + server + "/" + mode + "/" + docroot
						if tls {
							name += "/tls"
						}
						t.Run(name, func(t *testing.T) {
							c := testConfig(t)
							c.Web.PHP = php
							c.Web.Server = server
							c.Web.Docroot = docroot
							c.Multisite.Enabled = mode != "single"
							if mode != "single" {
								c.Multisite.Mode = mode
							}
							c.TLS.Enabled = tls
							c.Services.Redis = true
							c.Services.Adminer = true
							c.Services.Mailpit = true
							if err := renderTemplates(c); err != nil {
								t.Fatal(err)
							}
							b, _ := os.ReadFile(c.file("docker-compose.yml"))
							var parsed struct{ Services map[string]any }
							if err := yaml.Unmarshal(b, &parsed); err != nil {
								t.Fatal(err)
							}
							for _, svc := range []string{"php", "db", "caddy", "redis", "adminer", "mailpit"} {
								if parsed.Services[svc] == nil {
									t.Fatalf("missing %s", svc)
								}
							}
							if (parsed.Services["web"] != nil) != (server == "nginx") {
								t.Fatal("wrong web service")
							}
							dockerfile, _ := os.ReadFile(c.file(".wpdev", "generated", "php.Dockerfile"))
							for _, want := range []string{"imagick-3.8.1", "xdebug-3.5.3", "redis-6.3.0", "--with-avif", "PHP_VERSION_ID < 80500", "wp-cli-2.12.0.phar", "exec"} {
								if want == "exec" {
									continue
								}
								if !strings.Contains(string(dockerfile), want) {
									t.Errorf("missing %s", want)
								}
							}
							for _, bad := range []string{"libzip5", "libmagickcore-7.q16-10", "|| true"} {
								if strings.Contains(string(dockerfile), bad) {
									t.Errorf("obsolete %s", bad)
								}
							}
							if err := renderTemplates(c); err != nil {
								t.Fatal("render not idempotent", err)
							}
						})
					}
				}
			}
		}
	}
}
func TestLegacyUpgradeAndCustomPreservation(t *testing.T) {
	c := testConfig(t)
	fakeProcesses(t, func(cmd *exec.Cmd) error { return nil })
	for _, name := range templateNames {
		b, err := assets.ReadFile("assets/legacy/" + name)
		if err == nil {
			if err = atomicWrite(c.file(".wpdev", "templates", name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	legacy, _ := assets.ReadFile("assets/legacy/docker-compose.tmpl.yml")
	comp, err := executeTemplate("legacy", legacy, dataFor(c))
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(c.file("docker-compose.yml"), comp, 0o644); err != nil {
		t.Fatal(err)
	}
	custom := append(bundled("nginx.conf.tmpl"), []byte("\n# my custom setting\n")...)
	if err = atomicWrite(c.file(".wpdev", "templates", "nginx.conf.tmpl"), custom, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = renderTemplates(c); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(c.file(".wpdev", "templates", "nginx.conf.tmpl"))
	if string(got) != string(custom) {
		t.Fatal("overwrote user template")
	}
	php, _ := os.ReadFile(c.file(".wpdev", "templates", "php.Dockerfile.tmpl"))
	if string(php) != string(bundled("php.Dockerfile.tmpl")) {
		t.Fatal("legacy template not upgraded")
	}
	b, _ := os.ReadFile(c.file("docker-compose.yml"))
	b = append(b, []byte("\n# manual change\n")...)
	os.WriteFile(c.file("docker-compose.yml"), b, 0o644)
	if err = renderTemplates(c); err == nil {
		t.Fatal("overwrote manually edited compose")
	}
}
func TestFailedRenderLeavesActiveFiles(t *testing.T) {
	c := testConfig(t)
	fakeProcesses(t, func(cmd *exec.Cmd) error { return nil })
	if err := renderTemplates(c); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(c.file("docker-compose.yml"))
	oldPHP, _ := os.ReadFile(c.file(".wpdev", "generated", "php.Dockerfile"))
	if err := atomicWrite(c.file(".wpdev", "templates", "Caddyfile.http.tmpl"), []byte("{{ broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := renderTemplates(c); err == nil {
		t.Fatal("invalid template accepted")
	}
	after, _ := os.ReadFile(c.file("docker-compose.yml"))
	if string(before) != string(after) {
		t.Fatal("changed active compose")
	}
	after, _ = os.ReadFile(c.file(".wpdev", "generated", "php.Dockerfile"))
	if string(oldPHP) != string(after) {
		t.Fatal("changed active PHP config")
	}
}
func TestComposeValidationFailure(t *testing.T) {
	c := testConfig(t)
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		io.WriteString(cmd.Stderr, "invalid compose model")
		return errors.New("exit 1")
	})
	if err := renderTemplates(c); err == nil {
		t.Fatal("validation failure ignored")
	}
	if _, err := os.Stat(c.file("docker-compose.yml")); !os.IsNotExist(err) {
		t.Fatal("published invalid compose")
	}
}
func TestTemplateUpdateCustomNewAndReset(t *testing.T) {
	c := testConfig(t)
	if err := saveConfig(c.ConfigPath, c); err != nil {
		t.Fatal(err)
	}
	oldCfg := cfgFile
	cfgFile = c.ConfigPath
	t.Cleanup(func() { cfgFile = oldCfg })
	custom := []byte("# user owns this template\n")
	p := c.file(".wpdev", "templates", "nginx.conf.tmpl")
	atomicWrite(p, custom, 0o644)
	update := templatesCmd.Commands()[0]
	update.SetOut(io.Discard)
	t.Cleanup(func() {
		update.Flags().Set("apply", "false")
		update.Flags().Set("reset-custom", "false")
		update.SetOut(nil)
	})
	if err := update.RunE(update, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".new"); !os.IsNotExist(err) {
		t.Fatal("preview mutated files")
	}
	update.Flags().Set("apply", "true")
	if err := update.RunE(update, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != string(custom) {
		t.Fatal("custom overwritten")
	}
	b, _ = os.ReadFile(p + ".new")
	if string(b) != string(bundled("nginx.conf.tmpl")) {
		t.Fatal("missing replacement")
	}
	update.Flags().Set("reset-custom", "true")
	if err := update.RunE(update, nil); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != string(bundled("nginx.conf.tmpl")) {
		t.Fatal("reset not applied")
	}
}
func TestWordPressLoaderDoesNotHideUserCode(t *testing.T) {
	c := testConfig(t)
	p := c.file(c.Web.Docroot, "wp-content", "mu-plugins", "wpdev-local.php")
	atomicWrite(p, []byte("<?php // mine"), 0o644)
	if err := ensureWordPressLoader(c); err == nil {
		t.Fatal("user code overwritten")
	}
	os.Remove(p)
	if err := ensureWordPressLoader(c); err != nil {
		t.Fatal(err)
	}
	if err := ensureWordPressLoader(c); err != nil {
		t.Fatal("not idempotent", err)
	}
}

func TestExplicitLegacyUpdateKeepsComposeOwnership(t *testing.T) {
	c := testConfig(t)
	if err := saveConfig(c.ConfigPath, c); err != nil {
		t.Fatal(err)
	}
	for _, n := range templateNames {
		b, err := assets.ReadFile("assets/legacy/" + n)
		if err == nil {
			if err = atomicWrite(c.file(".wpdev", "templates", n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	src, _ := assets.ReadFile("assets/legacy/docker-compose.tmpl.yml")
	b, err := executeTemplate("legacy", src, dataFor(c))
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(c.file("docker-compose.yml"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	old := cfgFile
	cfgFile = c.ConfigPath
	t.Cleanup(func() { cfgFile = old })
	update := templatesCmd.Commands()[0]
	update.SetOut(io.Discard)
	update.Flags().Set("apply", "true")
	t.Cleanup(func() { update.SetOut(nil); update.Flags().Set("apply", "false") })
	if err = update.RunE(update, nil); err != nil {
		t.Fatal(err)
	}
	fakeProcesses(t, func(cmd *exec.Cmd) error { return nil })
	if err = renderTemplates(c); err != nil {
		t.Fatal("explicit upgrade lost generated Compose ownership", err)
	}
}
