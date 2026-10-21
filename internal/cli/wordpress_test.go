package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestNetworkMismatchAndHostValidation(t *testing.T) {
	c := testConfig(t)
	c.Multisite.Enabled = true
	s := wpNetworkState{Installed: true, Multisite: true, Domain: c.Domain, BasePath: "/"}
	if err := checkNetwork(c, s); err != nil {
		t.Fatal(err)
	}
	s.Subdomain = true
	if err := checkNetwork(c, s); err == nil {
		t.Fatal("silently changed network mode")
	}
	c.Services.Mailpit = true
	for _, h := range []string{"mail.example.test", "external.test", "a.b.example.test"} {
		if err := validateSiteHost(c, h); err == nil {
			t.Fatal("accepted", h)
		}
	}
	if err := validateSiteHost(c, "shop.example.test"); err != nil {
		t.Fatal(err)
	}
}
func TestHTAccessPreservesProjectSettings(t *testing.T) {
	c := testConfig(t)
	c.Multisite.Enabled = true
	original := "SetEnv REMOTE_MEDIA_URL https://media.example.org\n# BEGIN WordPress\nold rules\n# END WordPress\n# outside block\n"
	if err := atomicWrite(c.file(c.Web.Docroot, ".htaccess"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := plannedHTAccess(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SetEnv REMOTE_MEDIA_URL", "# outside block", "# BEGIN WordPress Multisite", "wp-admin"} {
		if !strings.Contains(string(b), want) {
			t.Fatal("lost", want)
		}
	}
	if strings.Contains(string(b), "old rules") {
		t.Fatal("old block retained")
	}
	atomicWrite(c.file(c.Web.Docroot, ".htaccess"), b, 0o644)
	again, err := plannedHTAccess(c)
	if err != nil || string(again) != string(b) {
		t.Fatal("not idempotent", err)
	}
}
func TestWPPassthroughArguments(t *testing.T) {
	c := testConfig(t)
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		a := strings.Join(cmd.Args, "|")
		for _, want := range []string{"--path=/var/www/html/wp", "--title=A title with spaces", "--url=https://example.test/"} {
			if !strings.Contains(a, want) {
				t.Fatal("lost argument", want, a)
			}
		}
		if cmd.Dir != c.Root {
			t.Fatal("wrong project directory")
		}
		return nil
	})
	if err := runWP(c, nil, os.Stdout, "site", "create", "--title=A title with spaces", "--url=https://example.test/"); err != nil {
		t.Fatal(err)
	}
}

func TestConversionStopsWhenBackupFails(t *testing.T) {
	c := testConfig(t)
	c.Multisite.Enabled = true
	if err := saveConfig(c.ConfigPath, c); err != nil {
		t.Fatal(err)
	}
	atomicWrite(c.file(c.Web.Docroot, "wp-load.php"), []byte("<?php"), 0o644)
	atomicWrite(c.file(c.Web.Docroot, "wp-config.php"), []byte("<?php // existing config"), 0o600)
	old := cfgFile
	cfgFile = c.ConfigPath
	t.Cleanup(func() { cfgFile = old })
	converted := false
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		args := strings.Join(cmd.Args, " ")
		switch {
		case strings.Contains(args, "core is-installed"):
			return nil
		case strings.Contains(args, " eval "):
			_, err := io.WriteString(cmd.Stdout, `{"installed":true,"multisite":false,"domain":"example.test","base_path":"/"}`)
			return err
		case strings.Contains(args, "mariadb-dump"):
			return errors.New("database disconnected")
		case strings.Contains(args, "multisite-convert"):
			converted = true
		}
		return nil
	})
	cmd := installNetworkCommand(true)
	cmd.Flags().Set("title", "Local")
	if err := cmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "backup failed") {
		t.Fatal("failed backup did not stop conversion", err)
	}
	if converted {
		t.Fatal("database conversion ran after backup failure")
	}
	b, _ := os.ReadFile(c.file(c.Web.Docroot, "wp-config.php"))
	if string(b) != "<?php // existing config" {
		t.Fatal("changed original config")
	}
}

func TestSyncRetriesAfterComposeFailure(t *testing.T) {
	c := testConfig(t)
	c.Multisite.Enabled = true
	c.Multisite.Mode = "subdomain"
	atomicWrite(c.file(c.Web.Docroot, "wp-load.php"), []byte("<?php"), 0o644)
	atomicWrite(c.file(c.Web.Docroot, "wp-config.php"), []byte("<?php"), 0o644)
	attempts := 0
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		args := strings.Join(cmd.Args, " ")
		switch {
		case strings.Contains(args, "core is-installed"):
			return nil
		case strings.Contains(args, " eval "):
			io.WriteString(cmd.Stdout, `{"installed":true,"multisite":true,"subdomain":true,"domain":"example.test","base_path":"/"}`)
		case strings.Contains(args, "site list"):
			io.WriteString(cmd.Stdout, `[{"domain":"example.test","path":"/"},{"domain":"child.example.test","path":"/"}]`)
		case strings.Contains(args, "up -d"):
			attempts++
			if attempts == 1 {
				return errors.New("Docker temporarily unavailable")
			}
		}
		return nil
	})
	if err := syncMultisite(c, true); err == nil {
		t.Fatal("ignored failed container update")
	}
	if err := syncMultisite(c, true); err != nil {
		t.Fatal("retry failed", err)
	}
	if attempts != 2 {
		t.Fatal("unchanged site list incorrectly skipped recovery")
	}
}
