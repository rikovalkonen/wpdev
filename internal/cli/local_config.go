package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func importAsset(name string) []byte {
	b, err := assets.ReadFile("assets/import/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

type configTransformation struct {
	Source string `json:"source"`
	Hints  struct {
		Mode string `json:"mode"`
	} `json:"hints"`
}

func localDBValues() map[string]any {
	return map[string]any{"DB_HOST": "db", "DB_NAME": "wordpress", "DB_USER": "wp", "DB_PASSWORD": "secret"}
}

// Use the project's PHP version, not a host PHP installation. No project files
// are mounted and the source travels through stdin, never process arguments.
func transformConfig(c *Config, source []byte, values map[string]any) (configTransformation, error) {
	var result configTransformation
	dir, err := os.MkdirTemp("", "wpdev-config-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	if err = os.WriteFile(filepath.Join(dir, "config.php"), importAsset("config.php"), 0o600); err != nil {
		return result, err
	}
	input, err := json.Marshal(map[string]any{"source": string(source), "values": values})
	if err != nil {
		return result, err
	}
	var out bytes.Buffer
	err = process(c.Root, bytes.NewReader(input), &out, "docker", "run", "--rm", "-i", "--network=none",
		"--mount", "type=bind,src="+dir+",dst=/wpdev,readonly",
		"php:"+c.Web.PHP+"-cli", "php", "/wpdev/config.php")
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(out.Bytes(), &result)
	return result, err
}

func configSource(c *Config) ([]byte, os.FileMode, error) {
	p := c.file(c.Web.Docroot, "wp-config.php")
	info, err := os.Lstat(p)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s must be a regular local file", p)
	}
	b, err := os.ReadFile(p)
	return b, info.Mode().Perm(), err
}

var localConfigCmd = &cobra.Command{Use: "config", Short: "Manage local WordPress configuration"}

func init() {
	cmd := &cobra.Command{Use: "local", Args: cobra.NoArgs, Short: "Use local DB credentials only inside wpdev containers", RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		b, mode, err := configSource(c)
		if err != nil {
			return err
		}
		result, err := transformConfig(c, b, localDBValues())
		if err != nil {
			return err
		}
		if bytes.Equal(b, []byte(result.Source)) {
			fmt.Fprintln(cmd.OutOrStdout(), "Local configuration is already current.")
			return nil
		}
		p := c.file(c.Web.Docroot, "wp-config.php")
		if err = backupFile(c, p); err != nil {
			return err
		}
		if err = atomicWrite(p, []byte(result.Source), mode); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Local DB settings enabled when WPDEV_LOCAL=1. Original expressions retained.")
		return nil
	}}
	localConfigCmd.AddCommand(cmd)
}
