// Package ops holds the operations on a stack that the CLI and the web
// interface share.
package ops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"stackwire/internal/catalog"
	"stackwire/internal/dockerx"
	"stackwire/internal/render"
	"stackwire/internal/stack"
	"stackwire/internal/storage"
	"stackwire/internal/wire"
)

// Stack is a loaded stack: its settings, the selected apps and its secrets.
type Stack struct {
	Cfg     *stack.Config
	Apps    []catalog.App
	Secrets *stack.Secrets
}

// Load reads the stack and makes sure every secret the apps need exists.
func Load(dir string) (*Stack, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	cfg, err := stack.Load(abs)
	if err != nil {
		return nil, err
	}
	apps, err := catalog.Resolve(cfg.Apps)
	if err != nil {
		return nil, err
	}
	secrets, err := stack.LoadSecrets(abs)
	if err != nil {
		return nil, err
	}
	if err := secrets.EnsureAdmin(); err != nil {
		return nil, err
	}
	for _, a := range apps {
		for name, kind := range a.Secrets {
			if err := secrets.Ensure(name, kind); err != nil {
				return nil, err
			}
		}
	}
	if err := secrets.Save(); err != nil {
		return nil, err
	}
	return &Stack{cfg, apps, secrets}, nil
}

// Render writes everything that must exist before the containers start.
func (s *Stack) Render(out io.Writer) (*wire.Ctx, error) {
	if err := render.Write(s.Cfg, s.Apps); err != nil {
		return nil, err
	}
	ctx := wire.NewCtx(s.Cfg, s.Secrets, s.Apps, out)
	if err := wire.Prepare(ctx); err != nil {
		return nil, err
	}
	return ctx, render.FixOwnership(s.Cfg, s.Apps)
}

// CheckProject fails if containers of the stack's compose project were
// started from another folder. Two stacks with the same name would otherwise
// treat each other's containers as their own, and applying one would remove
// the other's apps.
func CheckProject(cfg *stack.Config) error {
	dirs, err := dockerx.ProjectDirs(cfg.Name)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if !samePath(d, cfg.Dir) {
			return fmt.Errorf("the name %q is already used by the stack in %s; give this stack another name in %s",
				cfg.Name, d, filepath.Join(cfg.Dir, stack.ConfigFile))
		}
	}
	return nil
}

// CheckPorts fails if a port the stack publishes is taken by another
// program, or by two apps of the stack, before anything is started. Ports
// held by the stack's own running containers are fine.
func CheckPorts(s *Stack) error {
	ports, err := render.HostPorts(s.Cfg, s.Apps)
	if err != nil {
		return err
	}
	own, err := dockerx.ProjectPorts(s.Cfg.Name)
	if err != nil {
		return err
	}
	return checkPorts(ports, s.Apps, func(port int, proto string) bool {
		return own[fmt.Sprintf("%d/%s", port, proto)] || portFree(port, proto)
	})
}

func checkPorts(ports []render.HostPort, apps []catalog.App, free func(port int, proto string) bool) error {
	names := map[string]string{render.ProxyService: "the HTTPS proxy"}
	for _, a := range apps {
		names[a.ID] = a.Name
	}
	key := func(port int, proto string) string { return fmt.Sprintf("%d/%s", port, proto) }
	used := map[string]string{}
	var problems []string
	for _, p := range ports {
		k := key(p.Port, p.Proto)
		if other, ok := used[k]; ok {
			if other != p.App {
				problems = append(problems, fmt.Sprintf("port %s is set for both %s and %s; give one of them another port under \"ports\" in %s",
					k, names[other], names[p.App], stack.ConfigFile))
			}
			continue
		}
		used[k] = p.App
	}
	for _, p := range ports {
		if free(p.Port, p.Proto) {
			continue
		}
		msg := fmt.Sprintf("port %s for %s is in use by another program", key(p.Port, p.Proto), names[p.App])
		if p.Settable {
			alt := 0
			for c := p.Port + 1; c <= 65535 && c < p.Port+200; c++ {
				if _, taken := used[key(c, "tcp")]; !taken && free(c, "tcp") {
					alt = c
					break
				}
			}
			if alt != 0 {
				msg += fmt.Sprintf("; pick another one in the web interface or in %s, e.g.\n    ports:\n        %s: %d", stack.ConfigFile, p.App, alt)
			} else {
				msg += fmt.Sprintf("; pick another one in the web interface or under \"ports\" in %s", stack.ConfigFile)
			}
		} else {
			msg += "; stop that program, this port cannot be changed"
		}
		problems = append(problems, msg)
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// portFree reports whether a port can be opened on all interfaces.
func portFree(port int, proto string) bool {
	addr := fmt.Sprintf(":%d", port)
	if proto == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Up brings the running stack in line with the configuration: it starts the
// selected apps, removes the ones no longer selected and configures them.
func Up(s *Stack, out io.Writer, timeout time.Duration) error {
	if err := CheckProject(s.Cfg); err != nil {
		return err
	}
	if !render.HasServices(s.Cfg, s.Apps) {
		fmt.Fprintln(out, "No apps selected, removing the containers.")
		return dockerx.Project(s.Cfg.Name, out, "down", "--remove-orphans")
	}
	if err := CheckPorts(s); err != nil {
		return err
	}
	oldCaddyfile, _ := os.ReadFile(render.CaddyfilePath(s.Cfg))
	ctx, err := s.Render(out)
	if err != nil {
		return err
	}
	if len(s.Apps) > 0 {
		// The proxy starts last: a fresh *arr app lets the first visitor create
		// the login, so nothing is reachable from outside before that is set.
		if err := storage.Prepare(s.Cfg, s.Secrets, s.Apps, out); err != nil {
			return err
		}
		up := append([]string{"up", "-d", "--remove-orphans"}, render.AppServices(s.Apps)...)
		if err := dockerx.Compose(s.Cfg.Dir, out, up...); err != nil {
			return err
		}
		fmt.Fprintln(out, "\nWaiting for the apps:")
		if err := wire.Wait(ctx, timeout); err != nil {
			return err
		}
		fmt.Fprintln(out, "\nConfiguring:")
		if err := wire.Run(ctx); err != nil {
			return err
		}
	}
	if s.Cfg.Domain != "" {
		if err := dockerx.Compose(s.Cfg.Dir, out, "up", "-d", "--remove-orphans"); err != nil {
			return err
		}
		// Compose leaves a running proxy alone when only the Caddyfile
		// changed, and Caddy reads it at start only.
		newCaddyfile, _ := os.ReadFile(render.CaddyfilePath(s.Cfg))
		if oldCaddyfile != nil && !bytes.Equal(oldCaddyfile, newCaddyfile) {
			if err := dockerx.Compose(s.Cfg.Dir, out, "restart", render.ProxyService); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(out)
	Summary(s, out)
	return nil
}

// StartProxy starts only the proxy, so the web interface is reachable under
// the domain before any app is installed or while the apps are stopped.
// It reports false if the settings have to be applied first.
func StartProxy(s *Stack, out io.Writer) (bool, error) {
	if s.Cfg.Domain == "" {
		return false, nil
	}
	if err := CheckProject(s.Cfg); err != nil {
		return false, err
	}
	// Existing files are left alone: they describe what is running, and the
	// web interface compares them with the settings to show pending changes.
	if _, err := os.Stat(filepath.Join(s.Cfg.Dir, render.ComposeFile)); err != nil {
		if _, err := s.Render(out); err != nil {
			return false, err
		}
	} else if !render.UpToDate(s.Cfg, s.Apps) {
		return false, nil
	}
	err := dockerx.Compose(s.Cfg.Dir, out, "up", "-d", "--no-deps", render.ProxyService)
	return err == nil, err
}

// Wire configures the apps of a running stack again.
func Wire(s *Stack, out io.Writer, timeout time.Duration) error {
	ctx := wire.NewCtx(s.Cfg, s.Secrets, s.Apps, out)
	if err := wire.Wait(ctx, timeout); err != nil {
		return err
	}
	return wire.Run(ctx)
}

// Down stops and removes the containers. Data and configuration are kept.
func Down(s *Stack, out io.Writer) error {
	if err := CheckProject(s.Cfg); err != nil {
		return err
	}
	return dockerx.Project(s.Cfg.Name, out, "down", "--remove-orphans")
}

// Summary prints where the apps are and how to log in.
func Summary(s *Stack, out io.Writer) {
	if len(s.Apps) == 0 {
		fmt.Fprintln(out, "No apps are installed.")
	} else {
		fmt.Fprintln(out, "Your stack is running:")
	}
	for _, a := range s.Apps {
		if a.Internal {
			continue
		}
		fmt.Fprintf(out, "  %-14s %s\n", a.Name, s.Cfg.PublicURL(a.ID, a.Subdomain, a.DefaultHostPort()))
	}
	if u := render.UIURL(s.Cfg); u != "" {
		fmt.Fprintf(out, "  %-14s %s (while `stackwire serve` runs)\n", "stackwire", u)
	}
	fmt.Fprintf(out, "\nLogin for every app: user %q, password in %s (%s).\n",
		s.Secrets.Get(stack.AdminUser), filepath.Join(s.Cfg.Dir, stack.SecretsFile), stack.AdminPassword)
	if s.Cfg.Domain != "" {
		fmt.Fprintf(out, "DNS: point *.%s (or each app's subdomain) to this server; ports 80 and 443 must be open.\n", s.Cfg.Domain)
	}
	for _, a := range s.Apps {
		if a.Todo != "" {
			fmt.Fprintf(out, "Left to do in %s: %s\n", a.Name, a.Todo)
		}
	}
}
