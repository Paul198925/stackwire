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
	"slices"
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
	Name     string               `yaml:"name"`
	Services map[string]service   `yaml:"services"`
	Volumes  map[string]volumeDef `yaml:"volumes,omitempty"`
}

// volumeDef declares a Docker volume. Network storage volumes are created
// by stackwire before the apps start (see storage.Prepare), so compose
// only refers to them.
type volumeDef struct {
	Name     string `yaml:"name,omitempty"`
	External bool   `yaml:"external,omitempty"`
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
	Volume   *struct {
		Subpath string `yaml:"subpath"`
	} `yaml:"volume,omitempty"`
}

// Write generates all files for the stack and creates its directories.
func Write(cfg *stack.Config, apps []catalog.App) error {
	b, err := Compose(cfg, apps)
	if err != nil {
		return err
	}
	if err := checkStorage(cfg); err != nil {
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
		cf.Volumes = map[string]volumeDef{}
		for n := range named {
			cf.Volumes[n] = volumeDef{}
		}
		for _, s := range cfg.Storage {
			if s.Type == stack.StorageFolder {
				continue
			}
			if n := cfg.VolumeName(&s); cf.Volumes[n] == (volumeDef{}) {
				if _, used := named[n]; used {
					cf.Volumes[n] = volumeDef{Name: n, External: true}
				}
			}
		}
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
			cf.Volumes = map[string]volumeDef{}
		}
		cf.Volumes["caddy_data"], cf.Volumes["caddy_config"] = volumeDef{}, volumeDef{}
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
			// The whole data folder holds media and downloads. Kept apart,
			// each is mounted on its own at the same place inside.
			if sub == "" && cfg.SplitDownloads() {
				for _, part := range append([]string{"media"}, stack.DownloadFolders...) {
					pv := dataVolume(cfg, part)
					if pv.Type == "volume" {
						named[pv.Source] = struct{}{}
					}
					pv.Target, pv.ReadOnly = target+"/"+part, readOnly
					out = append(out, pv)
				}
				continue
			}
			v = dataVolume(cfg, sub)
			if v.Type == "volume" {
				named[v.Source] = struct{}{}
			}
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

// dataVolume mounts a folder below the data folder from wherever its area
// lives: the data folder, a drive, or a network share.
func dataVolume(cfg *stack.Config, sub string) volume {
	loc := cfg.Locate(sub)
	switch {
	case loc.Storage == nil:
		return volume{Type: "bind", Source: joinSub(composePath(loc.Root), loc.Rel)}
	case loc.Storage.Type == stack.StorageFolder:
		return volume{Type: "bind", Source: joinSub(loc.Root, loc.Rel)}
	}
	v := volume{Type: "volume", Source: cfg.VolumeName(loc.Storage)}
	if p := joinSub(loc.Root, loc.Rel); p != "" {
		v.Volume = &struct {
			Subpath string `yaml:"subpath"`
		}{strings.Trim(p, "/")}
	}
	return v
}

func joinSub(base, sub string) string {
	if base == "" {
		return sub
	}
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

// HostPort is a port the stack publishes on the host.
type HostPort struct {
	Port     int
	Proto    string // "tcp" or "udp"
	App      string // app ID, or ProxyService
	Settable bool   // the web UI port, which `ports` in stackwire.yaml can move
}

// HostPorts lists the ports the stack publishes on the host, as written to
// docker-compose.yml.
func HostPorts(cfg *stack.Config, apps []catalog.App) ([]HostPort, error) {
	var out []HostPort
	for _, a := range apps {
		out = append(out, HostPort{cfg.HostPort(a.ID, a.DefaultHostPort()), "tcp", a.ID, true})
		for _, spec := range a.ExtraPorts {
			p, err := parsePortSpec(spec)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", a.ID, err)
			}
			p.App = a.ID
			out = append(out, p)
		}
	}
	if cfg.Domain != "" {
		out = append(out, HostPort{80, "tcp", ProxyService, false}, HostPort{443, "tcp", ProxyService, false}, HostPort{443, "udp", ProxyService, false})
	}
	return out, nil
}

// parsePortSpec reads the host side of a compose port like "6881:6881/udp".
func parsePortSpec(spec string) (HostPort, error) {
	p := HostPort{Proto: "tcp"}
	s := spec
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s, p.Proto = s[:i], s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return p, fmt.Errorf("port %q has no host port", spec)
	}
	n, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil || n < 1 || n > 65535 {
		return p, fmt.Errorf("port %q has no valid host port", spec)
	}
	p.Port = n
	return p, nil
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
		// Drives with FAT, exFAT or NTFS have no owners; their mount options
		// decide who may write, and the storage check tells if the apps can.
		if err := os.Chown(d.Path, cfg.PUID, cfg.PGID); err != nil && !d.External {
			return err
		}
	}
	return nil
}

func dirs(cfg *stack.Config, apps []catalog.App) []string {
	var out []string
	for _, d := range dataDirs(cfg, apps) {
		out = append(out, d.Path)
	}
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

// dataSubs lists every folder below the data folder that the apps use,
// with all their parents, "" (the media area itself) first.
func dataSubs(cfg *stack.Config, apps []catalog.App) []string {
	seen := map[string]bool{"": true}
	out := []string{""}
	add := func(d string) {
		p := ""
		for _, part := range strings.Split(filepath.ToSlash(d), "/") {
			p = joinSub(p, part)
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	for _, a := range apps {
		for _, d := range append(append([]string{}, a.DataDirs...), volumeSubdirs(a, "data")...) {
			add(d)
		}
		// With downloads apart, the whole data folder is mounted in parts,
		// and each part must exist.
		if cfg.SplitDownloads() && mountsWholeData(a) {
			for _, d := range append([]string{"media"}, stack.DownloadFolders...) {
				add(d)
			}
		}
	}
	return out
}

func mountsWholeData(a catalog.App) bool {
	for _, spec := range a.Volumes {
		if src, _, _ := strings.Cut(spec, ":"); src == "data" {
			return true
		}
	}
	return false
}

// hostDir is a data folder on this machine. External ones are on a drive
// of the person's choosing, whose filesystem may not support ownership.
type hostDir struct {
	Path     string
	External bool
}

// dataDirs lists the data folders to create on this machine: in the data
// folder and on folder storages. Folders on network shares are created by
// storage.Prepare through Docker.
func dataDirs(cfg *stack.Config, apps []catalog.App) []hostDir {
	var out []hostDir
	seen := map[string]bool{}
	for _, sub := range dataSubs(cfg, apps) {
		loc := cfg.Locate(sub)
		var d hostDir
		switch {
		case loc.Storage == nil:
			d = hostDir{Path: filepath.Join(cfg.Abs(loc.Root), filepath.FromSlash(loc.Rel))}
		case loc.Storage.Type == stack.StorageFolder:
			d = hostDir{Path: filepath.Join(filepath.FromSlash(loc.Root), filepath.FromSlash(loc.Rel)), External: true}
		default:
			continue
		}
		if !seen[d.Path] {
			seen[d.Path] = true
			out = append(out, d)
		}
	}
	return out
}

// NetworkDirs lists, per network storage volume, the folders inside the
// share that the apps use.
func NetworkDirs(cfg *stack.Config, apps []catalog.App) map[string][]string {
	out := map[string][]string{}
	for _, sub := range dataSubs(cfg, apps) {
		loc := cfg.Locate(sub)
		if loc.Storage == nil || loc.Storage.Type == stack.StorageFolder {
			continue
		}
		name := cfg.VolumeName(loc.Storage)
		if p := strings.Trim(joinSub(loc.Root, loc.Rel), "/"); p != "" && !slices.Contains(out[name], p) {
			out[name] = append(out[name], p)
		} else if _, ok := out[name]; !ok {
			out[name] = nil
		}
	}
	return out
}

// checkStorage fails if a drive an area lives on is not there. Creating the
// folders anyway would quietly fill the system disk below the mount point.
func checkStorage(cfg *stack.Config) error {
	for _, a := range stack.Areas {
		p, ok := cfg.Areas[a.ID]
		if !ok || p.Storage == "" || p.Storage == stack.DataFolder {
			continue
		}
		s := cfg.StorageByID(p.Storage)
		if s.Type != stack.StorageFolder {
			continue
		}
		if fi, err := os.Stat(s.Path); err != nil || !fi.IsDir() {
			return fmt.Errorf("%s are on %s (%s), which is not there; connect the drive or choose another place under Settings > Storage",
				a.Name, storageName(s), s.Path)
		}
	}
	return nil
}

func storageName(s *stack.Storage) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
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
