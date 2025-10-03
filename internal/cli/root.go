package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "wpdev",
	Short: "Local WordPress dev environment manager",
	Long:  "wpdev is a simple CLI to spin up a local WordPress stack using Docker.",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is .wpdev.yml)")
	cobra.OnInitialize(initConfig)

	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(rebuildCmd)
	rootCmd.AddCommand(xdebugCmd)

	rootCmd.AddCommand(dbCmd) // parent for db:dump/import
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.SetConfigName(".wpdev")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
	}

	_ = viper.ReadInConfig() // ok if missing (init creates it)

	// Ensure .wpdev directory exists for artifacts
	_ = os.MkdirAll(filepath.Join(".wpdev", "db"), 0o755)
}
