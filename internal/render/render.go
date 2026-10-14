// Package render turns a stack configuration into docker-compose.yml, the
// reverse proxy configuration and the directory layout on disk.
package render

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

const (
	ComposeFile  = "docker-compose.yml"
	ProxyService = "proxy"

	// In domain mode the proxy also serves the web interface, which runs on
	// the host (`stackwire serve`), under this subdomain.
	UISubdomain = "stackwire"
	UIPort      = 8900
	// Name under which the proxy container reaches the host.
	hostAlias = "host.docker.internal"
)

type composeFile struct {
	Name     string              `yaml:"name"`
	Services map[string]service  `yaml:"services"`
	Volumes  map[string]struct{} `yaml:"volumes,omitempty"`
}

type service struct {
	Image       string            `yaml:"image"`
	Restart     string            `yaml:"restart"`
	Environment map[string]string `yaml:"environment,omitempty"`
	User        string            `yaml:"user,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"`
	Volumes     []volume          `yaml:"volumes,omitempty"`
	ExtraHosts  []string          `yaml:"extra_hosts,omitempty"`
	DependsOn   []string          `yaml:"depends_on,omitempty"`
	ShmSize     string            `yaml:"shm_size,omitempty"`
}

type volume struct {
	Type     string `yaml:"type"`
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"read_only,omitempty"`
}

// Write generates all files for the stack and creates its directories.
func Write(cfg *stack.Config, apps []catalog.App) error {
	b, err := Compose(cfg, apps)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfg.Dir, ComposeFile), b, 0o644); err != nil {
		return err
	}
	if cfg.Domain != "" {
		dir := filepath.Join(cfg.Dir, "caddy")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(CaddyfilePath(cfg), []byte(Caddyfile(cfg, apps)), 0o644); err != nil {
			return err
		}
	}
	for _, d := range dirs(cfg, apps) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// Compose renders docker-compose.yml.
func Compose(cfg *stack.Config, apps []catalog.App) ([]byte, error) {
	cf := composeFile{Name: cfg.Name, Services: map[string]service{}}
	// In domain mode only the proxy is public; the web ports stay on loopback
	// so the CLI can still reach the apps to configure them.
	bind := ""
	if cfg.Domain != "" {
		bind = "127.0.0.1:"
	}
	named := map[string]struct{}{}
	for _, a := range apps {
		if a.ID == ProxyService || a.Subdomain == UISubdomain {
			return nil, fmt.Errorf("app %q uses a reserved name", a.ID)
		}
		// Checked here too, since stackwire.yaml can be edited by hand.
		if err := a.CheckSettings(cfg.AppSettings[a.ID]); err != nil {
			return nil, fmt.Errorf("appSettings: %w", err)
		}
		env := map[string]string{
			"TZ":   cfg.Timezone,
			"PUID": strconv.Itoa(cfg.PUID),
			"PGID": strconv.Itoa(cfg.PGID),
		}
		addEnv(env, cfg, a, a.Env)
		s := service{
			Image:       a.Image,
			Restart:     "unless-stopped",
			Environment: env,
		}
		// An internal app is published on loopback only: the CLI needs to
		// reach it, people and other machines do not.
		appBind := bind
		if a.Internal {
			appBind = "127.0.0.1:"
		}
		s.Ports = []string{fmt.Sprintf("%s%d:%d", appBind, cfg.HostPort(a.ID, a.DefaultHostPort()), a.Port)}
		if a.RunAsUser {
			s.User = fmt.Sprintf("%d:%d", cfg.PUID, cfg.PGID)
		}
		s.Ports = append(s.Ports, a.ExtraPorts...)
		vols, err := volumes(cfg, a.ID, a.Volumes, named)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a.ID, err)
		}
		s.Volumes = vols
		// Sidecars are the app's own database, cache and so on. They are
		// only reachable inside the stack's network.
		for _, sc := range a.Sidecars {
			name := SidecarService(a, sc)
			if _, taken := cf.Services[name]; taken {
				return nil, fmt.Errorf("%s: service name %q is used twice", a.ID, name)
			}
			scEnv := map[string]string{"TZ": cfg.Timezone}
			addEnv(scEnv, cfg, a, sc.Env)
			scVols, err := volumes(cfg, a.ID, sc.Volumes, named)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", a.ID, sc.Name, err)
			}
			cf.Services[name] = service{Image: sc.Image, Restart: "unless-stopped", Environment: scEnv, Volumes: scVols, ShmSize: sc.ShmSize}
			s.DependsOn = append(s.DependsOn, name)
		}
		cf.Services[a.ID] = s
	}
	if len(named) > 0 {
		cf.Volumes = named
	}
	if cfg.Domain != "" {
		var deps []string
		for _, a := range apps {
			deps = append(deps, a.ID)
		}
		sort.Strings(deps)
		cf.Services[ProxyService] = service{
			Image:   "caddy:2",
			Restart: "unless-stopped",
			Ports:   []string{"80:80", "443:443", "443:443/udp"},
			Volumes: []volume{
				{Type: "bind", Source: "./caddy/Caddyfile", Target: "/etc/caddy/Caddyfile", ReadOnly: true},
				{Type: "volume", Source: "caddy_data", Target: "/data"},
				{Type: "volume", Source: "caddy_config", Target: "/config"},
			},
			ExtraHosts: []string{hostAlias + ":host-gateway"},
			DependsOn:  deps,
		}
		if cf.Volumes == nil {
			cf.Volumes = map[string]struct{}{}
		}
		cf.Volumes["caddy_data"], cf.Volumes["caddy_config"] = struct{}{}, struct{}{}
	}
	b, err := yaml.Marshal(cf)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Generated by stackwire from stackwire.yaml. Changes here are overwritten.\n"), b...), nil
}

// SidecarService is the compose service name of an app's sidecar.
func SidecarService(a catalog.App, sc catalog.Sidecar) string {
	return a.ID + "-" + sc.Name
}

// addEnv copies manifest variables into env. Values may use placeholders for
// settings of the stack. A variable whose value ends up empty is left out,
// so an app falls back to its own default.
func addEnv(env map[string]string, cfg *stack.Config, a catalog.App, vars map[string]string) {
	publicURL := ""
	if cfg.Domain != "" {
		publicURL = cfg.PublicURL(a.ID, a.Subdomain, a.DefaultHostPort())
	}
	pairs := []string{
		"{{publicURL}}", publicURL,
		"{{timezone}}", cfg.Timezone,
		"{{puid}}", strconv.Itoa(cfg.PUID),
		"{{pgid}}", strconv.Itoa(cfg.PGID),
	}
	for _, s := range a.Settings {
		pairs = append(pairs, "{{setting:"+s.Key+"}}", a.SettingValue(s.Key, cfg.AppSettings[a.ID]))
	}
	r := strings.NewReplacer(pairs...)
	for k, v := range vars {
		if v = r.Replace(v); v != "" {
			env[k] = v
		} else {
			delete(env, k)
		}
	}
}

// volumes turns manifest volume specs into compose volumes:
//
//	config:/target      the app's config folder
//	config/sub:/target  a folder inside it
//	data:/target        the data folder
//	data/sub:/target    a folder inside it
//	...:/target:ro      read-only
//	volume/name:/target a Docker volume named <app>-<name>, for databases:
//	                    they need a filesystem with Unix permissions, which
//	                    a bind mount on Windows or macOS does not provide
//
// Docker volumes are added to named.
func volumes(cfg *stack.Config, appID string, specs []string, named map[string]struct{}) ([]volume, error) {
	var out []volume
	for _, spec := range specs {
		src, target, ok := strings.Cut(spec, ":")
		if !ok || !strings.HasPrefix(target, "/") {
			return nil, fmt.Errorf("volume %q must be source:/path", spec)
		}
		target, readOnly := strings.CutSuffix(target, ":ro")
		kind, sub, _ := strings.Cut(src, "/")
		if strings.Contains(sub, "..") {
			return nil, fmt.Errorf("volume %q leaves its folder", spec)
		}
		var v volume
		switch kind {
		case "config":
			v = volume{Type: "bind", Source: joinSub(composePath(cfg.ConfigDir)+"/"+appID, sub)}
		case "data":
			v = volume{Type: "bind", Source: joinSub(composePath(cfg.DataDir), sub)}
		case "volume":
			if sub == "" {
				return nil, fmt.Errorf("volume %q needs a name", spec)
			}
			v = volume{Type: "volume", Source: appID + "-" + sub}
			named[v.Source] = struct{}{}
		default:
			return nil, fmt.Errorf("unknown volume source %q", kind)
		}
		v.Target, v.ReadOnly = target, readOnly
		out = append(out, v)
	}
	return out, nil
}

func joinSub(base, sub string) string {
	if sub == "" {
		return base
	}
	return base + "/" + sub
}

// Caddyfile renders the reverse proxy configuration for domain mode.
func Caddyfile(cfg *stack.Config, apps []catalog.App) string {
	var b strings.Builder
	b.WriteString("# Generated by stackwire. Changes here are overwritten.\n")
	if cfg.Email != "" {
		fmt.Fprintf(&b, "{\n\temail %s\n}\n", cfg.Email)
	}
	fmt.Fprintf(&b, "\n%s.%s {\n\treverse_proxy %s:%d\n}\n", UISubdomain, cfg.Domain, hostAlias, UIPort)
	for _, a := range apps {
		if a.Internal {
			continue
		}
		fmt.Fprintf(&b, "\n%s.%s {\n\treverse_proxy %s:%d\n}\n", a.Subdomain, cfg.Domain, a.ID, a.Port)
	}
	return b.String()
}

// UIURL is the address of the web interface in domain mode, or "".
func UIURL(cfg *stack.Config) string {
	if cfg.Domain == "" {
		return ""
	}
	return fmt.Sprintf("https://%s.%s", UISubdomain, cfg.Domain)
}

// HasServices reports whether the stack has anything to run. In domain mode
// the proxy runs even without apps, so the web interface stays reachable.
func HasServices(cfg *stack.Config, apps []catalog.App) bool {
	return len(apps) > 0 || cfg.Domain != ""
}

// UpToDate reports whether the generated files on disk match the
// configuration, i.e. whether there are settings that were not applied yet.
func UpToDate(cfg *stack.Config, apps []catalog.App) bool {
	if !HasServices(cfg, apps) {
		return true
	}
	want, err := Compose(cfg, apps)
	if err != nil {
		return false
	}
	have, err := os.ReadFile(filepath.Join(cfg.Dir, ComposeFile))
	if err != nil || !bytes.Equal(have, want) {
		return false
	}
	if cfg.Domain == "" {
		return true
	}
	have, err = os.ReadFile(CaddyfilePath(cfg))
	return err == nil && string(have) == Caddyfile(cfg, apps)
}

// CaddyfilePath is where the proxy configuration is written in domain mode.
func CaddyfilePath(cfg *stack.Config) string {
	return filepath.Join(cfg.Dir, "caddy", "Caddyfile")
}

// AppServices lists the compose services of the apps, without the proxy.
func AppServices(apps []catalog.App) []string {
	var out []string
	for _, a := range apps {
		out = append(out, a.ID)
	}
	return out
}

// FixOwnership hands the stack's directories to the container user when the
// CLI runs as root, which is the usual case on a fresh server.
func FixOwnership(cfg *stack.Config, apps []catalog.App) error {
	if runtime.GOOS == "windows" || os.Geteuid() != 0 {
		return nil
	}
	// Config directories are small, so they are walked. The data directory
	// can hold a whole library; only the directories created here are touched.
	err := filepath.WalkDir(cfg.Abs(cfg.ConfigDir), func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, cfg.PUID, cfg.PGID)
	})
	if err != nil {
		return err
	}
	for _, d := range dataDirs(cfg, apps) {
		if err := os.Chown(d, cfg.PUID, cfg.PGID); err != nil {
			return err
		}
	}
	return nil
}

func dirs(cfg *stack.Config, apps []catalog.App) []string {
	out := dataDirs(cfg, apps)
	for _, a := range apps {
		base := filepath.Join(cfg.Abs(cfg.ConfigDir), a.ID)
		out = append(out, base)
		for _, sub := range volumeSubdirs(a, "config") {
			out = append(out, filepath.Join(base, sub))
		}
	}
	return out
}

// volumeSubdirs lists the folders below kind ("config" or "data") that the
// app's volumes use.
func volumeSubdirs(a catalog.App, kind string) []string {
	specs := append([]string{}, a.Volumes...)
	for _, sc := range a.Sidecars {
		specs = append(specs, sc.Volumes...)
	}
	var out []string
	for _, spec := range specs {
		src, _, _ := strings.Cut(spec, ":")
		if k, sub, ok := strings.Cut(src, "/"); ok && k == kind && sub != "" {
			out = append(out, sub)
		}
	}
	return out
}

func dataDirs(cfg *stack.Config, apps []catalog.App) []string {
	root := cfg.Abs(cfg.DataDir)
	seen := map[string]bool{root: true}
	out := []string{root}
	for _, a := range apps {
		for _, d := range append(append([]string{}, a.DataDirs...), volumeSubdirs(a, "data")...) {
			// Add each parent as well so ownership is fixed along the whole path.
			p := root
			for _, part := range strings.Split(filepath.ToSlash(d), "/") {
				p = filepath.Join(p, part)
				if !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// composePath writes a host path the way compose expects it: relative paths
// start with ./ and are resolved against the directory of the compose file.
func composePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	p = strings.TrimSuffix(filepath.ToSlash(p), "/")
	if !strings.HasPrefix(p, "./") && !strings.HasPrefix(p, "../") {
		p = "./" + p
	}
	return p
}
