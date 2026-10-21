package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Kept at the process boundary so command construction and failure paths can be tested without Docker.
var executeProcess = func(c *exec.Cmd) error { return c.Run() }

func process(dir string, in io.Reader, out io.Writer, name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Stdin = in
	c.Stdout = out
	var stderr bytes.Buffer
	c.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := executeProcess(c); err != nil {
		return fmt.Errorf("%s failed: %w\n%s", name, err, stderr.String())
	}
	return nil
}
func composeArgs(c *Config, args ...string) []string {
	result := []string{"compose", "--project-directory", c.Root, "-f", c.file("docker-compose.yml")}
	for _, name := range []string{"docker-compose.override.yml", "docker-compose.override.yaml"} {
		if _, err := os.Stat(c.file(name)); err == nil {
			result = append(result, "-f", c.file(name))
			break
		}
	}
	return append(result, args...)
}
func compose(c *Config, in io.Reader, out io.Writer, args ...string) error {
	return process(c.Root, in, out, "docker", composeArgs(c, args...)...)
}
func captureCompose(c *Config, args ...string) (string, error) {
	var b bytes.Buffer
	err := compose(c, nil, &b, args...)
	return b.String(), err
}
