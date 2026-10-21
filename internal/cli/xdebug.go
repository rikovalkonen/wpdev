package cli

import (
	"fmt"
	"github.com/spf13/cobra"
)

var xdebugCmd = &cobra.Command{Use: "xdebug [on|off]", Args: cobra.ExactArgs(1), Short: "Enable or disable the installed Xdebug extension's modes", RunE: func(cmd *cobra.Command, args []string) error {
	if args[0] != "on" && args[0] != "off" {
		return fmt.Errorf("use xdebug on or off")
	}
	c, err := projectConfig()
	if err != nil {
		return err
	}
	c.Xdebug.Enabled = args[0] == "on"
	if err = saveConfig(c.ConfigPath, c); err != nil {
		return err
	}
	return startProject(c, false)
}}
