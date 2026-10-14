// Command stackwire sets up a self-hosted app stack on Docker and connects
// the apps to each other.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"stackwire/internal/catalog"
	"stackwire/internal/dockerx"
	"stackwire/internal/render"
	"stackwire/internal/stack"
	"stackwire/internal/wire"
)

const usage = `stackwire sets up a self-hosted app stack and wires the apps together.

Usage:
  stackwire <command> [flags]

Commands:
  apps      List the apps in the catalog
  init      Create stackwire.yaml in a directory
  render    Write docker-compose.yml and config files without starting anything
  up        Start the stack and configure the apps
  wire      Configure the apps of a running stack again
  status    Show the apps, their URLs and whether they answer
  down      Stop the stack (data and configuration are kept)

Every command takes --dir (default: current directory).
Run "stackwire <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"apps": cmdApps, "init": cmdInit, "render": cmdRender, "up": cmdUp,
		"wire": cmdWire, "status": cmdStatus, "down": cmdDown,
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		if a := os.Args[1]; a == "-h" || a == "--help" || a == "help" {
			fmt.Print(usage)
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("stackwire "+name, flag.ContinueOnError)
	dir := fs.String("dir", ".", "stack directory")
	return fs, dir
}

type loaded struct {
	cfg     *stack.Config
	apps    []catalog.App
	secrets *stack.Secrets
}

// load reads the stack and makes sure every secret the apps need exists.
func load(dir string) (*loaded, error) {
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
	return &loaded{cfg, apps, secrets}, nil
}

// renderAll writes everything that must exist before the containers start.
func renderAll(l *loaded) (*wire.Ctx, error) {
	if err := render.Write(l.cfg, l.apps); err != nil {
		return nil, err
	}
	ctx := wire.NewCtx(l.cfg, l.secrets, l.apps, os.Stdout)
	if err := wire.Prepare(ctx); err != nil {
		return nil, err
	}
	return ctx, render.FixOwnership(l.cfg, l.apps)
}

func cmdApps(args []string) error {
	fs := flag.NewFlagSet("stackwire apps", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	apps, err := catalog.All()
	if err != nil {
		return err
	}
	for _, a := range apps {
		fmt.Printf("%-12s %-8s %s\n", a.ID, a.Category, a.Description)
	}
	return nil
}

func cmdInit(args []string) error {
	fs, dir := newFlags("init")
	domain := fs.String("domain", "", "base domain; apps get https://<app>.<domain>. Empty: reach apps by port")
	email := fs.String("email", "", "contact address for Let's Encrypt (with --domain)")
	apps := fs.String("apps", "", "comma-separated app IDs (default: the media stack)")
	data := fs.String("data", "", "directory for media and downloads (default: ./data)")
	tz := fs.String("tz", "", "timezone, e.g. Europe/Berlin (default: detected)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(abs, stack.ConfigFile)); err == nil {
		return fmt.Errorf("%s already exists in %s", stack.ConfigFile, abs)
	}
	cfg := stack.Default(abs)
	cfg.Domain, cfg.Email = strings.ToLower(*domain), *email
	if *apps != "" {
		cfg.Apps = strings.Split(*apps, ",")
	}
	if *data != "" {
		cfg.DataDir = *data
	}
	if *tz != "" {
		cfg.Timezone = *tz
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if _, err := catalog.Resolve(cfg.Apps); err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\nNext: stackwire up --dir %s\n", filepath.Join(abs, stack.ConfigFile), *dir)
	return nil
}

func cmdRender(args []string) error {
	fs, dir := newFlags("render")
	if err := fs.Parse(args); err != nil {
		return err
	}
	l, err := load(*dir)
	if err != nil {
		return err
	}
	if _, err := renderAll(l); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", filepath.Join(l.cfg.Dir, render.ComposeFile))
	return nil
}

func cmdUp(args []string) error {
	fs, dir := newFlags("up")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the apps to become ready")
	if err := fs.Parse(args); err != nil {
		return err
	}
	l, err := load(*dir)
	if err != nil {
		return err
	}
	ctx, err := renderAll(l)
	if err != nil {
		return err
	}
	// The proxy starts last: a fresh *arr app lets the first visitor create
	// the login, so nothing is reachable from outside before that is set.
	up := append([]string{"up", "-d", "--remove-orphans"}, render.AppServices(l.apps)...)
	if err := dockerx.Compose(l.cfg.Dir, up...); err != nil {
		return err
	}
	fmt.Println("\nWaiting for the apps:")
	if err := wire.Wait(ctx, *timeout); err != nil {
		return err
	}
	fmt.Println("\nConfiguring:")
	if err := wire.Run(ctx); err != nil {
		return err
	}
	if l.cfg.Domain != "" {
		if err := dockerx.Compose(l.cfg.Dir, "up", "-d"); err != nil {
			return err
		}
	}
	fmt.Println()
	printSummary(l)
	return nil
}

func cmdWire(args []string) error {
	fs, dir := newFlags("wire")
	timeout := fs.Duration("timeout", time.Minute, "how long to wait for the apps to become ready")
	if err := fs.Parse(args); err != nil {
		return err
	}
	l, err := load(*dir)
	if err != nil {
		return err
	}
	ctx := wire.NewCtx(l.cfg, l.secrets, l.apps, os.Stdout)
	if err := wire.Wait(ctx, *timeout); err != nil {
		return err
	}
	return wire.Run(ctx)
}

func cmdStatus(args []string) error {
	fs, dir := newFlags("status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	l, err := load(*dir)
	if err != nil {
		return err
	}
	ctx := wire.NewCtx(l.cfg, l.secrets, l.apps, os.Stdout)
	for _, a := range l.apps {
		state := "down"
		if wire.Healthy(ctx, a) {
			state = "up"
		}
		fmt.Printf("%-12s %-5s %s\n", a.ID, state, l.cfg.PublicURL(a.ID, a.Subdomain, a.Port))
	}
	return nil
}

func cmdDown(args []string) error {
	fs, dir := newFlags("down")
	if err := fs.Parse(args); err != nil {
		return err
	}
	l, err := load(*dir)
	if err != nil {
		return err
	}
	return dockerx.Compose(l.cfg.Dir, "down")
}

func printSummary(l *loaded) {
	fmt.Println("Your stack is running:")
	for _, a := range l.apps {
		fmt.Printf("  %-12s %s\n", a.Name, l.cfg.PublicURL(a.ID, a.Subdomain, a.Port))
	}
	fmt.Printf("\nLogin for every app: user %q, password in %s (%s).\n",
		l.secrets.Get(stack.AdminUser), filepath.Join(l.cfg.Dir, stack.SecretsFile), stack.AdminPassword)
	if l.cfg.Domain != "" {
		fmt.Printf("DNS: point *.%s (or each app's subdomain) to this server; ports 80 and 443 must be open.\n", l.cfg.Domain)
	}
	for _, a := range l.apps {
		if a.Driver == "prowlarr" {
			fmt.Printf("Left to do: add your indexers in Prowlarr. They sync to the other apps on their own.\n")
		}
	}
}
