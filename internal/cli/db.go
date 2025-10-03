package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Database utilities",
}

var dbDumpCmd = &cobra.Command{
	Use:   "dump",
	Short: "Dump database to .wpdev/db/dump.sql",
	RunE: func(cmd *cobra.Command, args []string) error {
		_ = os.MkdirAll(".wpdev/db", 0o755)
		path := filepath.Join(".wpdev", "db", fmt.Sprintf("dump-%s.sql", time.Now().Format("20060102-150405")))
		c := exec.Command("docker", "compose", "exec", "-T", "db",
			"sh", "-lc", "mysqldump -u root -proot --databases wordpress > /tmp/dump.sql && cat /tmp/dump.sql")
		out, err := c.Output()
		if err != nil { return err }
		if err := os.WriteFile(path, out, 0o644); err != nil { return err }
		fmt.Println("Wrote", path)
		return nil
	},
}

var dbImportCmd = &cobra.Command{
	Use:   "import <path.sql>",
	Short: "Import SQL dump into DB",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := args[0]
		f, err := os.Open(path)
		if err != nil { return err }
		defer f.Close()
		// Stream file into container to /tmp/dump.sql, then import.
		sh := "cat > /tmp/dump.sql && mysql -u root -proot < /tmp/dump.sql"
		c := exec.Command("docker", "compose", "exec", "-T", "db", "sh", "-lc", sh)
		c.Stdin = f
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	},
}

func init() {
	dbCmd.AddCommand(dbDumpCmd)
	dbCmd.AddCommand(dbImportCmd)
}
