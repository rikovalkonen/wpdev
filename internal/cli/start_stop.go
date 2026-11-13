package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"os"
	"os/exec"
	"strings"
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
	removeDetachedContainers(c)
	if err := compose(c, nil, os.Stdout, upArgs(rebuild)...); err != nil {
		return err
	}
	if !rebuild && phpImageStale(c) {
		fmt.Fprintln(os.Stderr, "Warning: the PHP image was built from an older Dockerfile, entrypoint or PHP version. Run wpdev rebuild to apply the current configuration.")
	}
	return nil
}

// start reuses existing images; Compose still builds one that is missing.
func upArgs(rebuild bool) []string {
	args := []string{"up", "-d", "--wait", "--wait-timeout", "180"}
	if rebuild {
		args = append(args, "--build", "--remove-orphans")
	}
	return args
}

// When a start fails on an occupied host port, Docker leaves the container
// without any network. Compose then starts it as-is on the next up: running,
// but unreachable and with no published ports. Removing such containers makes
// up recreate them. Best effort; a failure here must not block start.
func removeDetachedContainers(c *Config) {
	out, err := quietOutput(c.Root, "docker", composeArgs(c, "ps", "-a", "-q")...)
	ids := strings.Fields(string(out))
	if err != nil || len(ids) == 0 {
		return
	}
	args := append([]string{"inspect", "--format", `{{.ID}} {{.HostConfig.NetworkMode}} {{len .NetworkSettings.Networks}}`}, ids...)
	if out, err = quietOutput(c.Root, "docker", args...); err != nil {
		return
	}
	var detached []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[2] != "0" {
			continue
		}
		// These modes legitimately have no network entries.
		if f[1] == "none" || f[1] == "host" || strings.HasPrefix(f[1], "container:") {
			continue
		}
		detached = append(detached, f[0])
	}
	if len(detached) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "Recreating", len(detached), "container(s) that lost their network after a failed start.")
	quietOutput(c.Root, "docker", append([]string{"rm", "-f"}, detached...)...)
}

const buildLabel = "wpdev.build"

// Reports only a proven mismatch: custom templates without the label and
// Docker failures stay silent, because start must not fail on this check.
func phpImageStale(c *Config) bool {
	var model struct {
		Name     string `json:"name"`
		Services map[string]struct {
			Image string `json:"image"`
			Build struct {
				Labels map[string]string `json:"labels"`
			} `json:"build"`
		} `json:"services"`
	}
	out, err := quietOutput(c.Root, "docker", composeArgs(c, "config", "--format", "json")...)
	if err != nil || json.Unmarshal(out, &model) != nil {
		return false
	}
	php, ok := model.Services["php"]
	want := php.Build.Labels[buildLabel]
	if !ok || want == "" {
		return false
	}
	image := php.Image
	if image == "" {
		image = model.Name + "-php"
	}
	out, err = quietOutput(c.Root, "docker", "image", "inspect", "--format", `{{index .Config.Labels "`+buildLabel+`"}}`, image)
	if err != nil {
		return false
	}
	// An image without the label predates this check.
	return strings.TrimSpace(string(out)) != want
}
func quietOutput(dir, name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Stdout = dir, &out
	err := executeProcess(cmd)
	return out.Bytes(), err
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

var startCmd = &cobra.Command{Use: "start", Short: "Render and start the stack; builds images only when missing", RunE: func(cmd *cobra.Command, args []string) error {
	c, err := projectConfig()
	if err != nil {
		return err
	}
	if err = startProject(c, false); err != nil {
		return err
	}
	return applyPlugins(c, pluginApplyOptions{}, cmd.OutOrStdout())
}}
var rebuildCmd = &cobra.Command{Use: "rebuild", Short: "Render, rebuild images and restart the stack", RunE: func(cmd *cobra.Command, args []string) error {
	c, err := projectConfig()
	if err != nil {
		return err
	}
	if err = startProject(c, true); err != nil {
		return err
	}
	return applyPlugins(c, pluginApplyOptions{}, cmd.OutOrStdout())
}}
var stopCmd = &cobra.Command{Use: "stop", Short: "Stop the stack without deleting database volumes", RunE: func(cmd *cobra.Command, args []string) error {
	c, err := projectConfig()
	if err != nil {
		return err
	}
	return compose(c, nil, cmd.OutOrStdout(), "down")
}}
