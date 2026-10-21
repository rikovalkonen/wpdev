package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"os"
	"time"
)

var dbCmd = &cobra.Command{Use: "db", Short: "Stream database backups and imports"}

func dbTools(c *Config) (string, string) {
	if c.Database.Engine == "mysql" {
		return "mysqldump", "mysql"
	}
	return "mariadb-dump", "mariadb"
}
func dumpDatabase(c *Config) (string, error) {
	dir := c.file(".wpdev", "db")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".dump-*.partial")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	dump, _ := dbTools(c)
	err = compose(c, nil, f, "exec", "-T", "db", dump, "-uroot", "-proot", "--single-transaction", "--databases", "wordpress")
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	p := c.file(".wpdev", "db", "dump-"+time.Now().UTC().Format("20060102-150405.000000000")+".sql")
	if err = os.Rename(f.Name(), p); err != nil {
		return "", err
	}
	return p, nil
}
func init() {
	dbCmd.AddCommand(&cobra.Command{Use: "dump", Args: cobra.NoArgs, Short: "Back up the database", RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		p, err := dumpDatabase(c)
		if err == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "Wrote", p)
		}
		return err
	}})
	var opts importOptions
	var localize bool
	importCmd := &cobra.Command{Use: "import <path.sql[.gz]>", Args: cobra.ExactArgs(1), Short: "Import SQL; optionally localize production URLs and configuration", RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		if localize {
			return localizeDatabase(cmd, c, args[0], opts)
		}
		if opts.DryRun || opts.Yes || opts.Map != "" || opts.Prefix != "" || opts.Mode != "" {
			return fmt.Errorf("localization flags require --localize")
		}
		f, err := openSQL(args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		_, client := dbTools(c)
		return compose(c, f, cmd.OutOrStdout(), "exec", "-T", "db", client, "-uroot", "-proot", "wordpress")
	}}
	addImportFlags(importCmd, &opts)
	importCmd.Flags().BoolVar(&localize, "localize", false, "Convert production URLs and wp-config to this local project")
	dbCmd.AddCommand(importCmd)
	var localOpts importOptions
	localCmd := &cobra.Command{Use: "localize", Args: cobra.NoArgs, Short: "Localize the database already imported into this project", RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		return localizeDatabase(cmd, c, "", localOpts)
	}}
	addImportFlags(localCmd, &localOpts)
	dbCmd.AddCommand(localCmd)
}
