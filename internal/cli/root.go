package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "0.5"

var cfgFile string
var rootCmd = &cobra.Command{Use: "wpdev", Version: version, Short: "Local WordPress development with Docker", SilenceUsage: true, SilenceErrors: true}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default .wpdev.yml; paths are relative to its directory)")
	rootCmd.AddCommand(initCmd, startCmd, stopCmd, rebuildCmd, xdebugCmd, dbCmd, tlsCmd, templatesCmd, wpCmd, multisiteCmd, siteCmd, doctorCmd, localConfigCmd, pullCmd, pluginsCmd)
}
