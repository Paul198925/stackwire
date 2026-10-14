// Package dockerx runs docker compose for a stack.
package dockerx

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"stackwire/internal/render"
)

// Compose runs `docker compose <args>` in the stack directory and streams
// its output to the terminal.
func Compose(dir string, args ...string) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found in PATH; install Docker Engine or Docker Desktop first")
	}
	full := append([]string{"compose", "-f", filepath.Join(dir, render.ComposeFile), "--project-directory", dir}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose %s: %w", args[0], err)
	}
	return nil
}
