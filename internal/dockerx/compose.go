// Package dockerx runs docker compose for a stack.
package dockerx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"stackwire/internal/render"
)

// Compose runs `docker compose <args>` against the stack's compose file and
// streams the output to out.
func Compose(dir string, out io.Writer, args ...string) error {
	return run(out, append([]string{"compose", "-f", filepath.Join(dir, render.ComposeFile), "--project-directory", dir}, args...), args[0])
}

// Project runs `docker compose <args>` against a project by name. It works
// without a compose file, e.g. before the first render or after the last app
// was removed.
func Project(name string, out io.Writer, args ...string) error {
	return run(out, append([]string{"compose", "-p", name}, args...), args[0])
}

// ProjectOutput is like Project but returns what the command printed.
func ProjectOutput(name string, args ...string) ([]byte, error) {
	return Output(append([]string{"compose", "-p", name}, args...)...)
}

// Output runs `docker <args>` and returns what it printed.
func Output(args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, errNoDocker
	}
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return stdout.Bytes(), nil
}

var errNoDocker = errors.New("docker not found in PATH; install Docker Engine or Docker Desktop first")

func run(out io.Writer, args []string, verb string) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errNoDocker
	}
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose %s: %w", verb, err)
	}
	return nil
}
