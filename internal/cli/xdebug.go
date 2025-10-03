package cli

import (
	"fmt"
	"github.com/spf13/cobra"
)

var xdebugCmd = &cobra.Command{
	Use:   "xdebug [on|off]",
	Short: "Toggle Xdebug",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mode := args[0]
		cfg, err := loadConfig(".wpdev.yml")
		if err != nil { return err }
		if mode == "on" {
			cfg.Xdebug.Enabled = true
		} else if mode == "off" {
			cfg.Xdebug.Enabled = false
		} else {
			return fmt.Errorf("unknown mode %q, use on|off", mode)
		}
		if err := saveConfig(".wpdev.yml", cfg); err != nil { return err }
		fmt.Println("Xdebug set to", mode, "— rebuilding PHP container...")
		return rebuildCmd.RunE(cmd, nil)
	},
}
