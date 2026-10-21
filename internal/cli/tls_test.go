package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCA(t *testing.T) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "wpdev test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, key
}
func testLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, host string, until time.Time) ([]byte, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: until, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})
}
func TestCertificateValidation(t *testing.T) {
	root, ca, key := testCA(t)
	now := time.Now()
	leaf, priv := testLeaf(t, ca, key, "example.test", now.Add(90*24*time.Hour))
	if err := checkCertificate(leaf, priv, root, []string{"example.test"}, now); err != nil {
		t.Fatal(err)
	}
	other, _, _ := testCA(t)
	for _, tc := range []struct {
		name            string
		cert, key, root []byte
		host            string
	}{{"wrong root", leaf, priv, other, "example.test"}, {"wrong hostname", leaf, priv, root, "other.test"}, {"bad key", leaf, []byte("not PEM"), root, "example.test"}, {"wildcard absent", leaf, priv, root, "*.example.test"}} {
		t.Run(tc.name, func(t *testing.T) {
			if checkCertificate(tc.cert, tc.key, tc.root, []string{tc.host}, now) == nil {
				t.Fatal("accepted invalid certificate")
			}
		})
	}
	exp, expKey := testLeaf(t, ca, key, "example.test", now.Add(10*24*time.Hour))
	if checkCertificate(exp, expKey, root, []string{"example.test"}, now) == nil {
		t.Fatal("did not renew near expiry")
	}
}
func TestTLSLifecycle(t *testing.T) {
	c := testConfig(t)
	c.TLS.Enabled = true
	caroot := filepath.Join(t.TempDir(), "Application Support", "mkcert")
	root, ca, key := testCA(t)
	atomicWrite(filepath.Join(caroot, "rootCA.pem"), root, 0o644)
	atomicWrite(filepath.Join(caroot, "rootCA-key.pem"), []byte("never copy"), 0o600)
	generations := 0
	fakeProcesses(t, func(cmd *exec.Cmd) error {
		if cmd.Args[0] != "mkcert" {
			t.Fatal(cmd.Args)
		}
		switch cmd.Args[1] {
		case "-CAROOT":
			io.WriteString(cmd.Stdout, caroot+"\n")
		case "-install":
		case "-cert-file":
			generations++
			cert, priv := testLeaf(t, ca, key, cmd.Args[len(cmd.Args)-1], time.Now().Add(90*24*time.Hour))
			if err := os.WriteFile(cmd.Args[2], cert, 0o644); err != nil {
				return err
			}
			return os.WriteFile(cmd.Args[4], priv, 0o600)
		default:
			t.Fatal(cmd.Args)
		}
		return nil
	})
	if err := ensureTLS(c, true); err != nil {
		t.Fatal(err)
	}
	if generations != 2 {
		t.Fatal("missing certificates")
	}
	first := c.TLSFingerprint
	if err := ensureTLS(c, false); err != nil {
		t.Fatal(err)
	}
	if generations != 2 {
		t.Fatal("unnecessary regeneration")
	}
	root, ca, key = testCA(t)
	atomicWrite(filepath.Join(caroot, "rootCA.pem"), root, 0o644)
	if err := ensureTLS(c, false); err != nil {
		t.Fatal(err)
	}
	if generations != 4 || c.TLSFingerprint == first {
		t.Fatal("CA rotation not applied")
	}
	filepath.Walk(c.file(".wpdev"), func(p string, info os.FileInfo, err error) error {
		if strings.Contains(p, "rootCA-key") {
			t.Fatal("copied CA private key")
		}
		return nil
	})
	c.TLS.Enabled = false
	executeProcess = func(cmd *exec.Cmd) error { t.Fatal("called mkcert for HTTP"); return nil }
	if err := ensureTLS(c, false); err != nil {
		t.Fatal(err)
	}
}
