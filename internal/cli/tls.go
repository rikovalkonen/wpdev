package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

var tlsCmd = &cobra.Command{
	Use:   "tls",
	Short: "TLS utilities (mkcert)",
}

var tlsInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate local TLS certs with mkcert for your domain",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(".wpdev.yml")
		if err != nil {
			return err
		}
		if !cfg.TLS.Enabled {
			fmt.Println("tls.init skipped: tls.enabled is false in .wpdev.yml")
			return nil
		}
		if cfg.Domain == "" || cfg.Domain == "localhost" {
			return fmt.Errorf("set a valid domain in .wpdev.yml (e.g., myshop.test)")
		}

		// Skip if already present (idempotent)
		if certsPresent(cfg.Domain) {
			fmt.Println("TLS certs already exist; skipping generation.")
			return nil
		}

		// Do the work
		if err := generateCertsForDomain(cfg.Domain); err != nil {
			return err
		}
		fmt.Println("TLS init complete. Now run: wpdev start")
		return nil
	},
}

func init() {
	tlsCmd.AddCommand(tlsInitCmd)
	rootCmd.AddCommand(tlsCmd)
}

// ----- Helpers (same package; callable from init.go) -----

func generateCertsForDomain(domain string) error {
	// Ensure mkcert is installed
	if _, err := exec.LookPath("mkcert"); err != nil {
		return fmt.Errorf("mkcert not found. Install it first (brew install mkcert, choco install mkcert, etc.)")
	}

	certDir := filepath.Join(".wpdev", "certs")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
	 return err
	}

	// Install local CA (idempotent)
	c := exec.Command("mkcert", "-install")
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return err
	}

	// Generate leaf certs for domain and wildcard
	wild := "*." + domain

	certPath := filepath.Join(certDir, domain+".pem")
	keyPath := filepath.Join(certDir, domain+"-key.pem")
	wcCertPath := filepath.Join(certDir, "_wildcard."+domain+".pem")
	wcKeyPath := filepath.Join(certDir, "_wildcard."+domain+"-key.pem")

	fmt.Println("Generating certs for", domain, "and", wild)

	cmd1 := exec.Command("mkcert", "-cert-file", certPath, "-key-file", keyPath, domain)
	cmd1.Stdout, cmd1.Stderr = os.Stdout, os.Stderr
	if err := cmd1.Run(); err != nil {
		return err
	}

	cmd2 := exec.Command("mkcert", "-cert-file", wcCertPath, "-key-file", wcKeyPath, wild)
	cmd2.Stdout, cmd2.Stderr = os.Stdout, os.Stderr
	if err := cmd2.Run(); err != nil {
		return err
	}

	fmt.Println("Wrote:", certPath, keyPath, wcCertPath, wcKeyPath)
	return nil
}

func certsPresent(domain string) bool {
	need := []string{
		filepath.Join(".wpdev", "certs", domain+".pem"),
		filepath.Join(".wpdev", "certs", domain+"-key.pem"),
		filepath.Join(".wpdev", "certs", "_wildcard."+domain+".pem"),
		filepath.Join(".wpdev", "certs", "_wildcard."+domain+"-key.pem"),
	}
	for _, p := range need {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return false
		}
	}
	return true
}