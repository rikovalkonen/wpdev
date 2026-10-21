package cli

import (
	"bytes"
	"fmt"
	"github.com/spf13/cobra"
	"os"
)

func startProject(c *Config, rebuild bool) error {
	if err := startStack(c, rebuild); err != nil {
		return err
	}
	return checkStartedWordPress(c)
}

// Imports prepare the stack before production domains can pass network checks.
func startStack(c *Config, rebuild bool) error {
	if err := projectIgnore(c); err != nil {
		return err
	}
	if err := ensureTLS(c, false); err != nil {
		return err
	}
	if err := renderTemplates(c); err != nil {
		return err
	}
	if b, err := os.ReadFile(c.file(c.Web.Docroot, "wp-config.php")); err == nil {
		if bytes.Contains(b, []byte("/* wpdev:original ")) {
			if err := validateLocalRuntime(c); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	args := []string{"up", "-d", "--build", "--wait", "--wait-timeout", "180"}
	if rebuild {
		args = append(args, "--remove-orphans")
	}
	if err := compose(c, nil, os.Stdout, args...); err != nil {
		return err
	}
	return nil
}
func checkStartedWordPress(c *Config) error {
	if c.Multisite.Enabled && c.WordPress {
		state, err := networkState(c)
		if err != nil {
			return err
		}
		if state.Installed {
			if !state.Multisite {
				return fmt.Errorf("multisite is enabled in config but WordPress is single-site; run wpdev multisite convert")
			}
			return syncMultisite(c, true)
		}
		fmt.Println("WordPress is not installed yet. Run wpdev multisite install after preparing wp-config.php.")
	}
	return nil
}

var startCmd = &cobra.Command{Use: "start", Short: "Render, build changed images and start the stack", RunE: func(cmd *cobra.Command, args []string) error {
	c, err := projectConfig()
	if err != nil {
		return err
	}
	return startProject(c, false)
}}
var rebuildCmd = &cobra.Command{Use: "rebuild", Short: "Render and rebuild the stack", RunE: func(cmd *cobra.Command, args []string) error {
	c, err := projectConfig()
	if err != nil {
		return err
	}
	return startProject(c, true)
}}
var stopCmd = &cobra.Command{Use: "stop", Short: "Stop the stack without deleting database volumes", RunE: func(cmd *cobra.Command, args []string) error {
	c, err := projectConfig()
	if err != nil {
		return err
	}
	return compose(c, nil, cmd.OutOrStdout(), "down")
}}
