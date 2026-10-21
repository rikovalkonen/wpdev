package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func atomicCopy(source, dest string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err = os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dest), ".wpdev-copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if err = out.Chmod(mode); err == nil {
		_, err = io.Copy(out, in)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(out.Name(), dest)
}

func atomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".wpdev-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func writeIfMissing(p string, b []byte) error {
	if _, err := os.Stat(p); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicWrite(p, b, 0o644)
}
func backupFile(c *Config, p string) error {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(c.Root, p)
	if err != nil {
		return err
	}
	dest := c.file(".wpdev", "backups", time.Now().UTC().Format("20060102T150405.000000000"), rel)
	if err = atomicWrite(dest, b, 0o600); err != nil {
		return err
	}
	fmt.Println("Backup:", dest)
	return nil
}
