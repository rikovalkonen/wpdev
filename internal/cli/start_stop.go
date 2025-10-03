package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the local stack",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(".wpdev.yml")
		if err != nil { return err }

		// Ensure generated dir
		gen := filepath.Join(".wpdev", "generated")
		if err := os.MkdirAll(gen, 0o755); err != nil { return err }

		// Render nginx.conf and docker-compose.yml
		if err := renderTemplates(cfg); err != nil { return err }

		// docker compose up -d
		fmt.Println("Bringing up containers...")
		c := exec.Command("docker", "compose", "up", "-d")
		c.Stdout = os.Stdout; c.Stderr = os.Stderr
		return c.Run()
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the local stack",
	RunE: func(cmd *cobra.Command, args []string) error {
		c := exec.Command("docker", "compose", "down")
		c.Stdout = os.Stdout; c.Stderr = os.Stderr
		return c.Run()
	},
}

var rebuildCmd = &cobra.Command{
	Use:   "rebuild",
	Short: "Rebuild containers",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Re-render templates in case config changed
		cfg, err := loadConfig(".wpdev.yml")
		if err != nil { return err }
		if err := renderTemplates(cfg); err != nil { return err }

		c := exec.Command("docker", "compose", "up", "-d", "--build", "--remove-orphans")
		c.Stdout = os.Stdout; c.Stderr = os.Stderr
		return c.Run()
	},
}
