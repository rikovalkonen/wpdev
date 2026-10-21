package cli

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var tlsCmd = &cobra.Command{Use: "tls", Short: "Manage mkcert certificates and local CA trust"}

func init() {
	tlsCmd.AddCommand(&cobra.Command{Use: "init", Short: "Install the local CA and ensure valid project certificates", RunE: func(cmd *cobra.Command, args []string) error {
		c, err := projectConfig()
		if err != nil {
			return err
		}
		if !c.TLS.Enabled {
			fmt.Fprintln(cmd.OutOrStdout(), "TLS is disabled.")
			return nil
		}
		if err = ensureTLS(c, true); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "TLS ready. Run wpdev start to apply certificate changes to containers.")
		return nil
	}})
}
func certificate(b []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("invalid PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
func checkCertificate(certPEM, keyPEM, caPEM []byte, names []string, now time.Time) error {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("certificate/key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	ca, err := certificate(caPEM)
	if err != nil {
		return err
	}
	if !ca.IsCA {
		return fmt.Errorf("root certificate is not a CA")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now}); err != nil {
		return err
	}
	if leaf.NotAfter.Before(now.Add(30 * 24 * time.Hour)) {
		return fmt.Errorf("certificate expires in less than 30 days")
	}
	for _, name := range names {
		if strings.HasPrefix(name, "*.") {
			found := false
			for _, san := range leaf.DNSNames {
				if san == name {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("certificate is missing %s", name)
			}
		} else if err = leaf.VerifyHostname(name); err != nil {
			return err
		}
	}
	return nil
}
func verifyTLSFiles(c *Config, root []byte) error {
	for _, item := range []struct{ stem, host string }{{c.Domain, c.Domain}, {"_wildcard." + c.Domain, "*." + c.Domain}} {
		cert, err := os.ReadFile(c.file(".wpdev", "certs", item.stem+".pem"))
		if err != nil {
			return err
		}
		key, err := os.ReadFile(c.file(".wpdev", "certs", item.stem+"-key.pem"))
		if err != nil {
			return err
		}
		if err = checkCertificate(cert, key, root, []string{item.host}, time.Now()); err != nil {
			return fmt.Errorf("%s: %w", item.stem, err)
		}
	}
	return nil
}
func tlsFingerprint(c *Config) error {
	if !c.TLS.Enabled {
		c.TLSFingerprint = ""
		return nil
	}
	b := []byte{}
	for _, name := range []string{"rootCA.pem", c.Domain + ".pem", c.Domain + "-key.pem", "_wildcard." + c.Domain + ".pem", "_wildcard." + c.Domain + "-key.pem"} {
		part, err := os.ReadFile(c.file(".wpdev", "certs", name))
		if err != nil {
			return err
		}
		b = append(b, part...)
	}
	c.TLSFingerprint = hash(b)
	return nil
}
func ensureTLS(c *Config, install bool) error {
	if !c.TLS.Enabled {
		return nil
	}
	var out bytes.Buffer
	if err := process(c.Root, nil, &out, "mkcert", "-CAROOT"); err != nil {
		return fmt.Errorf("mkcert is required when tls.enabled is true: %w", err)
	}
	caroot := strings.TrimSpace(out.String())
	if !filepath.IsAbs(caroot) {
		return fmt.Errorf("mkcert -CAROOT did not return an absolute path")
	}
	rootPath := filepath.Join(caroot, "rootCA.pem")
	if _, err := os.Stat(rootPath); os.IsNotExist(err) {
		install = true
	} else if err != nil {
		return err
	}
	if install {
		if err := process(c.Root, nil, os.Stdout, "mkcert", "-install"); err != nil {
			return err
		}
	}
	root, err := os.ReadFile(rootPath)
	if err != nil {
		return err
	}
	ca, err := certificate(root)
	if err != nil {
		return err
	}
	if !ca.IsCA || time.Now().Before(ca.NotBefore) || !time.Now().Before(ca.NotAfter) {
		return fmt.Errorf("mkcert CA is invalid or expired")
	}
	dir := c.file(".wpdev", "certs")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err = verifyTLSFiles(c, root); err != nil {
		fmt.Println("Generating mkcert certificates:", err)
		stage, e := os.MkdirTemp(dir, ".renew-*")
		if e != nil {
			return e
		}
		defer os.RemoveAll(stage)
		for _, item := range []struct{ stem, host string }{{c.Domain, c.Domain}, {"_wildcard." + c.Domain, "*." + c.Domain}} {
			cp, kp := filepath.Join(stage, item.stem+".pem"), filepath.Join(stage, item.stem+"-key.pem")
			if e = process(c.Root, nil, os.Stdout, "mkcert", "-cert-file", cp, "-key-file", kp, item.host); e != nil {
				return e
			}
			cert, e := os.ReadFile(cp)
			if e != nil {
				return e
			}
			key, e := os.ReadFile(kp)
			if e != nil {
				return e
			}
			if e = checkCertificate(cert, key, root, []string{item.host}, time.Now()); e != nil {
				return e
			}
		}
		for _, stem := range []string{c.Domain, "_wildcard." + c.Domain} {
			for _, suffix := range []string{".pem", "-key.pem"} {
				b, e := os.ReadFile(filepath.Join(stage, stem+suffix))
				if e != nil {
					return e
				}
				mode := os.FileMode(0o644)
				if suffix == "-key.pem" {
					mode = 0o600
				}
				if e = atomicWrite(filepath.Join(dir, stem+suffix), b, mode); e != nil {
					return e
				}
			}
		}
	}
	if err = atomicWrite(filepath.Join(dir, "rootCA.pem"), root, 0o644); err != nil {
		return err
	}
	return tlsFingerprint(c)
}
