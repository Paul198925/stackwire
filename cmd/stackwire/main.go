// Command stackwire sets up a self-hosted app stack on Docker and connects
// the apps to each other.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"stackwire/internal/catalog"
	"stackwire/internal/ops"
	"stackwire/internal/render"
	"stackwire/internal/stack"
	"stackwire/internal/web"
	"stackwire/internal/wire"
)

const usage = `stackwire sets up a self-hosted app stack and wires the apps together.

Usage:
  stackwire <command> [flags]

Commands:
  serve     Run the web interface to manage the stack in the browser
  apps      List the apps in the catalog
  init      Create stackwire.yaml in a directory
  render    Write docker-compose.yml and config files without starting anything
  up        Start the stack and configure the apps
  wire      Configure the apps of a running stack again
  status    Show the apps, their URLs and whether they answer
  down      Stop the stack (data and configuration are kept)
  version   Print the version

Every command takes --dir (default: current directory).
Run "stackwire <command> -h" for the flags of a command.
`

// version is set at release builds with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"apps": cmdApps, "init": cmdInit, "render": cmdRender, "up": cmdUp,
		"wire": cmdWire, "status": cmdStatus, "down": cmdDown, "serve": cmdServe,
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		switch os.Args[1] {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return
		case "version", "--version":
			fmt.Println("stackwire", version)
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
	apps := fs.String("apps", "", "comma-separated app IDs, or \"none\" to choose them later in the web interface (default: the media stack)")
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
	if *apps == "none" {
		cfg.Apps = nil
	} else if *apps != "" {
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
	s, err := ops.Load(*dir)
	if err != nil {
		return err
	}
	if _, err := s.Render(os.Stdout); err != nil {
		return err
	}
	fmt.Printf("Wrote the stack files to %s\n", s.Cfg.Dir)
	return nil
}

func cmdUp(args []string) error {
	fs, dir := newFlags("up")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the apps to become ready")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := ops.Load(*dir)
	if err != nil {
		return err
	}
	return ops.Up(s, os.Stdout, *timeout)
}

func cmdWire(args []string) error {
	fs, dir := newFlags("wire")
	timeout := fs.Duration("timeout", time.Minute, "how long to wait for the apps to become ready")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := ops.Load(*dir)
	if err != nil {
		return err
	}
	return ops.Wire(s, os.Stdout, *timeout)
}

func cmdStatus(args []string) error {
	fs, dir := newFlags("status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := ops.Load(*dir)
	if err != nil {
		return err
	}
	ctx := wire.NewCtx(s.Cfg, s.Secrets, s.Apps, os.Stdout)
	for _, a := range s.Apps {
		state := "down"
		if wire.Healthy(ctx, a) {
			state = "up"
		}
		fmt.Printf("%-12s %-5s %s\n", a.ID, state, s.Cfg.PublicURL(a.ID, a.Subdomain, a.DefaultHostPort()))
	}
	return nil
}

func cmdDown(args []string) error {
	fs, dir := newFlags("down")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := ops.Load(*dir)
	if err != nil {
		return err
	}
	return ops.Down(s, os.Stdout)
}

func cmdServe(args []string) error {
	fs, dir := newFlags("serve")
	addr := fs.String("addr", "127.0.0.1:8900", "address to listen on; use 0.0.0.0:8900 to reach it from other devices")
	if err := fs.Parse(args); err != nil {
		return err
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	// A new directory starts empty; the apps are chosen in the browser.
	if _, err := os.Stat(filepath.Join(abs, stack.ConfigFile)); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return err
		}
		cfg := stack.Default(abs)
		cfg.Apps = nil
		if err := cfg.Save(); err != nil {
			return err
		}
		if _, err := web.StartSetup(abs); err != nil {
			return err
		}
		fmt.Printf("Created a new stack in %s\n", abs)
	}
	s, err := ops.Load(abs)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	ip := net.ParseIP(host)
	local := ip != nil && ip.IsLoopback()
	hosts := []string{host}
	switch {
	case local:
		hosts = []string{"localhost"}
	case ip == nil || ip.IsUnspecified():
		// Listening everywhere: show the addresses others can use.
		hosts = append(lanAddrs(), "localhost")
	}
	fmt.Printf("stackwire is running at http://%s\n", net.JoinHostPort(hosts[0], port))
	if code := web.SetupCode(abs); code != "" {
		fmt.Println("Open this link to set up the stack (the code in it is needed once):")
		for _, h := range hosts {
			fmt.Printf("  http://%s/?setup=%s\n", net.JoinHostPort(h, port), code)
		}
	} else {
		fmt.Printf("Sign in as %q with the password in %s (%s).\n",
			s.Secrets.Get(stack.AdminUser), filepath.Join(abs, stack.SecretsFile), stack.AdminPassword)
	}
	if !local {
		fmt.Println("Note: other devices can reach this page and the connection is not encrypted. Use it on a network you trust.")
	}
	handler := web.New(abs).Handler()
	if u := render.UIURL(s.Cfg); u != "" {
		if port != strconv.Itoa(render.UIPort) {
			fmt.Printf("Note: the proxy expects this page on port %d, so %s will not work with --addr %s.\n", render.UIPort, u, *addr)
		}
		// Without apps or after `down` no proxy runs, and the page would
		// only be reachable locally.
		switch started, err := ops.StartProxy(s, os.Stdout); {
		case err != nil:
			fmt.Printf("Could not start the proxy: %v\n", err)
		case started:
			fmt.Printf("With DNS for *.%s pointing here, it is also at %s\n", s.Cfg.Domain, u)
		default:
			fmt.Printf("Apply the pending changes to make it reachable at %s\n", u)
		}
	}
	// A domain can be set or removed in the browser at any time.
	go web.ServeProxyAccess(abs, handler, os.Stdout)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// lanAddrs returns this machine's IPv4 addresses other devices can reach,
// leaving out loopback and the bridges Docker creates.
func lanAddrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 ||
			strings.HasPrefix(ifc.Name, "docker") || strings.HasPrefix(ifc.Name, "br-") || strings.HasPrefix(ifc.Name, "veth") ||
			strings.Contains(ifc.Name, "vEthernet") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				out = append(out, n.IP.String())
			}
		}
	}
	return out
}
