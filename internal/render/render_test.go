package render

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

func mediaStack(t *testing.T) (*stack.Config, []catalog.App) {
	t.Helper()
	cfg := stack.Default(t.TempDir())
	apps, err := catalog.Resolve(cfg.Apps)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, apps
}

func parse(t *testing.T, cfg *stack.Config, apps []catalog.App) composeFile {
	t.Helper()
	b, err := Compose(cfg, apps)
	if err != nil {
		t.Fatal(err)
	}
	var cf composeFile
	if err := yaml.Unmarshal(b, &cf); err != nil {
		t.Fatal(err)
	}
	return cf
}

func TestComposeLANMode(t *testing.T) {
	cfg, apps := mediaStack(t)
	cfg.Ports = map[string]int{"qbittorrent": 8081}
	cf := parse(t, cfg, apps)

	if _, ok := cf.Services[ProxyService]; ok {
		t.Error("proxy present without a domain")
	}
	s := cf.Services["sonarr"]
	if s.Ports[0] != "8989:8989" {
		t.Errorf("sonarr ports = %v", s.Ports)
	}
	if s.Environment["SONARR__AUTH__APIKEY"] != "${SONARR_API_KEY}" || s.Environment["TZ"] == "" {
		t.Errorf("sonarr environment = %v", s.Environment)
	}
	want := []volume{
		{Type: "bind", Source: "./config/sonarr", Target: "/config"},
		{Type: "bind", Source: "./data", Target: "/data"},
	}
	if len(s.Volumes) != 2 || s.Volumes[0] != want[0] || s.Volumes[1] != want[1] {
		t.Errorf("sonarr volumes = %v", s.Volumes)
	}
	qb := cf.Services["qbittorrent"]
	if strings.Join(qb.Ports, " ") != "8081:8080 6881:6881 6881:6881/udp" {
		t.Errorf("qbittorrent ports = %v", qb.Ports)
	}
}

func TestComposeDomainMode(t *testing.T) {
	cfg, apps := mediaStack(t)
	cfg.Domain, cfg.Email = "example.com", "me@example.com"
	cf := parse(t, cfg, apps)

	if got := cf.Services["sonarr"].Ports[0]; got != "127.0.0.1:8989:8989" {
		t.Errorf("sonarr must only listen on loopback, got %s", got)
	}
	p, ok := cf.Services[ProxyService]
	if !ok || len(p.DependsOn) != len(apps) {
		t.Fatalf("proxy = %+v", p)
	}
	caddy := Caddyfile(cfg, apps)
	if len(p.ExtraHosts) != 1 || p.ExtraHosts[0] != "host.docker.internal:host-gateway" {
		t.Errorf("proxy cannot reach the host: extra_hosts = %v", p.ExtraHosts)
	}
	for _, want := range []string{"email me@example.com", "jellyfin.example.com {", "reverse_proxy sonarr:8989",
		"stackwire.example.com {", "reverse_proxy host.docker.internal:8900"} {
		if !strings.Contains(caddy, want) {
			t.Errorf("Caddyfile lacks %q:\n%s", want, caddy)
		}
	}
}

func TestDomainModeWithoutApps(t *testing.T) {
	cfg, _ := mediaStack(t)
	cfg.Domain = "example.com"
	// The proxy alone keeps the web interface reachable.
	cf := parse(t, cfg, nil)
	if _, ok := cf.Services[ProxyService]; !ok || len(cf.Services) != 1 {
		t.Fatalf("services = %v, want only the proxy", cf.Services)
	}
	if !strings.Contains(Caddyfile(cfg, nil), "stackwire.example.com {") {
		t.Error("Caddyfile lacks the web interface")
	}
	if UpToDate(cfg, nil) {
		t.Error("an unwritten proxy-only stack counts as up to date")
	}
	if err := Write(cfg, nil); err != nil {
		t.Fatal(err)
	}
	if !UpToDate(cfg, nil) {
		t.Error("stack is not up to date right after writing it")
	}
	cfg.Domain = ""
	if !UpToDate(cfg, nil) || HasServices(cfg, nil) {
		t.Error("a stack without apps and domain has nothing to apply")
	}
}

func TestInternalAndRunAsUserApps(t *testing.T) {
	cfg := stack.Default(t.TempDir())
	apps, err := catalog.Resolve([]string{"jellyfin", "prowlarr", "flaresolverr", "seerr"})
	if err != nil {
		t.Fatal(err)
	}
	// Without a domain the other apps listen on all interfaces.
	cf := parse(t, cfg, apps)
	if got := cf.Services["flaresolverr"].Ports[0]; got != "127.0.0.1:8191:8191" {
		t.Errorf("an app without login is published as %s", got)
	}
	if got := cf.Services["prowlarr"].Ports[0]; got != "9696:9696" {
		t.Errorf("prowlarr ports = %s", got)
	}
	if got := cf.Services["seerr"].User; got == "" || cf.Services["prowlarr"].User != "" {
		t.Errorf("user: seerr = %q, prowlarr = %q", got, cf.Services["prowlarr"].User)
	}
	cfg.Domain = "example.com"
	caddy := Caddyfile(cfg, apps)
	if strings.Contains(caddy, "flaresolverr.example.com") || !strings.Contains(caddy, "seerr.example.com {") {
		t.Errorf("proxy routes are wrong:\n%s", caddy)
	}
}

func TestSidecars(t *testing.T) {
	cfg := stack.Default(t.TempDir())
	apps, err := catalog.Resolve([]string{"immich", "paperless"})
	if err != nil {
		t.Fatal(err)
	}
	cf := parse(t, cfg, apps)
	db, ok := cf.Services["immich-database"]
	if !ok {
		t.Fatalf("no immich-database service: %v", cf.Services)
	}
	if len(db.Ports) != 0 {
		t.Errorf("a database is published: %v", db.Ports)
	}
	if db.ShmSize != "128mb" || db.Environment["POSTGRES_PASSWORD"] != "${IMMICH_DB_PASSWORD}" || db.Environment["PUID"] != "" {
		t.Errorf("immich-database = %+v", db)
	}
	if db.Volumes[0] != (volume{Type: "volume", Source: "immich-database", Target: "/var/lib/postgresql/data"}) {
		t.Errorf("database volume = %+v", db.Volumes)
	}
	if _, ok := cf.Volumes["immich-model-cache"]; !ok {
		t.Errorf("named volumes = %v", cf.Volumes)
	}
	if got := strings.Join(cf.Services["immich"].DependsOn, ","); got != "immich-machine-learning,immich-redis,immich-database" {
		t.Errorf("immich depends on %s", got)
	}
	p := cf.Services["paperless"]
	if p.Volumes[0].Source != "./config/paperless/data" || p.Volumes[2].Source != "./data/documents/inbox" {
		t.Errorf("paperless volumes = %+v", p.Volumes)
	}
	// Placeholders: no domain means no PAPERLESS_URL at all.
	if _, ok := p.Environment["PAPERLESS_URL"]; ok || p.Environment["USERMAP_UID"] != strconv.Itoa(cfg.PUID) {
		t.Errorf("paperless environment = %v", p.Environment)
	}
	cfg.Domain = "example.com"
	if got := parse(t, cfg, apps).Services["paperless"].Environment["PAPERLESS_URL"]; got != "https://paperless.example.com" {
		t.Errorf("PAPERLESS_URL = %q", got)
	}
	if strings.Contains(Caddyfile(cfg, apps), "immich-database") {
		t.Error("a sidecar got a proxy route")
	}
	cfg.Domain = ""
	if err := Write(cfg, apps); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"config/paperless/data", "data/documents/inbox", "data/photos"} {
		if _, err := os.Stat(filepath.Join(cfg.Dir, d)); err != nil {
			t.Errorf("missing %s", d)
		}
	}
}

func TestRequirements(t *testing.T) {
	if _, err := catalog.Resolve([]string{"seerr"}); err == nil {
		t.Error("seerr without jellyfin was accepted")
	}
	got, err := catalog.WithRequirements([]string{"sonarr", "seerr", "flaresolverr"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "sonarr,jellyfin,seerr,prowlarr,flaresolverr" {
		t.Errorf("WithRequirements = %v", got)
	}
	if _, err := catalog.Resolve(got); err != nil {
		t.Errorf("result does not resolve: %v", err)
	}
}

func TestWriteCreatesLayout(t *testing.T) {
	cfg, apps := mediaStack(t)
	if err := Write(cfg, apps); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ComposeFile, "config/sonarr", "data/media/tv", "data/media/movies", "data/torrents/tv", "data/torrents/movies"} {
		if _, err := os.Stat(filepath.Join(cfg.Dir, p)); err != nil {
			t.Errorf("missing %s", p)
		}
	}
}
