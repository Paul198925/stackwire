package wire

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

// fakeArr imitates the parts of the Servarr API the drivers use.
type fakeArr struct {
	mu      sync.Mutex
	key     string
	host    map[string]any
	lists   map[string][]map[string]any
	schemas map[string][]map[string]any
	writes  int
}

var apiPrefix = regexp.MustCompile(`^/api/v\d`)

func (f *fakeArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Api-Key") != f.key {
		http.Error(w, "bad key", http.StatusUnauthorized)
		return
	}
	path := apiPrefix.ReplaceAllString(r.URL.Path, "")
	var body map[string]any
	if r.Method != "GET" {
		f.writes++
		json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case path == "/config/host" && r.Method == "GET":
		json.NewEncoder(w).Encode(f.host)
	case path == "/config/host/1" && r.Method == "PUT":
		f.host = body
	case strings.HasSuffix(path, "/schema"):
		json.NewEncoder(w).Encode(f.schemas[strings.TrimSuffix(path, "/schema")])
	case r.Method == "GET":
		if f.lists[path] == nil {
			f.lists[path] = []map[string]any{}
		}
		json.NewEncoder(w).Encode(f.lists[path])
	case r.Method == "POST":
		f.lists[path] = append(f.lists[path], body)
		w.WriteHeader(http.StatusCreated)
	default:
		http.NotFound(w, r)
	}
}

func fields(names ...string) []any {
	var out []any
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

func newFakeArr(key, categoryField string) *fakeArr {
	return &fakeArr{
		key:   key,
		host:  map[string]any{"id": 1, "authenticationMethod": "none"},
		lists: map[string][]map[string]any{},
		schemas: map[string][]map[string]any{
			"/downloadclient": {
				{"implementation": "Transmission", "fields": fields("host")},
				{"implementation": "QBittorrent", "presets": []any{}, "fields": fields("host", "port", "username", "password", categoryField)},
			},
			"/notification": {
				{"implementation": "MediaBrowser", "supportsOnDownload": true, "supportsOnUpgrade": true,
					"fields": fields("host", "port", "useSsl", "apiKey", "updateLibrary")},
			},
			"/applications": {
				{"implementation": "Sonarr", "fields": fields("prowlarrUrl", "baseUrl", "apiKey")},
				{"implementation": "Radarr", "fields": fields("prowlarrUrl", "baseUrl", "apiKey")},
			},
		},
	}
}

func fieldValue(res map[string]any, name string) any {
	for _, f := range res["fields"].([]any) {
		if m := f.(map[string]any); m["name"] == name {
			return m["value"]
		}
	}
	return nil
}

// fakeJellyfin imitates the startup wizard, libraries and API keys.
type fakeJellyfin struct {
	mu         sync.Mutex
	wizardDone bool
	user, pass string
	gotUser    bool
	libraries  []string
	keys       []string
}

func (f *fakeJellyfin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	authed := strings.Contains(r.Header.Get("Authorization"), `Token="tok"`)
	switch r.Method + " " + r.URL.Path {
	case "GET /System/Info/Public":
		json.NewEncoder(w).Encode(map[string]any{"StartupWizardCompleted": f.wizardDone})
	case "POST /Startup/Configuration", "POST /Startup/RemoteAccess":
		w.WriteHeader(http.StatusNoContent)
	case "GET /Startup/User":
		f.gotUser = true
		json.NewEncoder(w).Encode(map[string]any{"Name": "root"})
	case "POST /Startup/User":
		if !f.gotUser {
			http.Error(w, "no initial user", http.StatusInternalServerError)
			return
		}
		f.user, f.pass = body["Name"].(string), body["Password"].(string)
		w.WriteHeader(http.StatusNoContent)
	case "POST /Startup/Complete":
		f.wizardDone = true
		w.WriteHeader(http.StatusNoContent)
	case "POST /Users/AuthenticateByName":
		if !f.wizardDone || body["Username"] != f.user || body["Pw"] != f.pass {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok"})
	case "GET /Library/VirtualFolders":
		var out []map[string]any
		for _, n := range f.libraries {
			out = append(out, map[string]any{"Name": strings.SplitN(n, "=", 2)[0]})
		}
		json.NewEncoder(w).Encode(out)
	case "POST /Library/VirtualFolders":
		if !authed {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		f.libraries = append(f.libraries, r.URL.Query().Get("name")+"="+r.URL.Query().Get("paths"))
		w.WriteHeader(http.StatusNoContent)
	case "GET /Auth/Keys":
		var items []map[string]any
		for _, k := range f.keys {
			items = append(items, map[string]any{"AppName": k, "AccessToken": "jfkey"})
		}
		json.NewEncoder(w).Encode(map[string]any{"Items": items})
	case "POST /Auth/Keys":
		if !authed {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		f.keys = append(f.keys, r.URL.Query().Get("app"))
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func TestRunWiresTheMediaStack(t *testing.T) {
	dir := t.TempDir()
	cfg := stack.Default(dir)
	apps, err := catalog.Resolve(cfg.Apps)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := stack.LoadSecrets(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.EnsureAdmin(); err != nil {
		t.Fatal(err)
	}
	for _, a := range apps {
		for name, kind := range a.Secrets {
			if err := secrets.Ensure(name, kind); err != nil {
				t.Fatal(err)
			}
		}
	}
	pass := secrets.Get(stack.AdminPassword)

	sonarr := newFakeArr(secrets.Get("SONARR_API_KEY"), "tvCategory")
	radarr := newFakeArr(secrets.Get("RADARR_API_KEY"), "movieCategory")
	prowl := newFakeArr(secrets.Get("PROWLARR_API_KEY"), "category")
	jf := &fakeJellyfin{}
	qbLogins := 0
	qb := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.URL.Path != "/api/v2/auth/login" || r.Form.Get("username") != "admin" || r.Form.Get("password") != pass {
			io.WriteString(w, "Fails.")
			return
		}
		qbLogins++
		io.WriteString(w, "Ok.")
	})

	ctx := NewCtx(cfg, secrets, apps, io.Discard)
	ctx.BaseURLs = map[string]string{}
	for id, h := range map[string]http.Handler{"sonarr": sonarr, "radarr": radarr, "prowlarr": prowl, "jellyfin": jf, "qbittorrent": qb} {
		srv := httptest.NewServer(h)
		defer srv.Close()
		ctx.BaseURLs[id] = srv.URL
	}

	if err := Run(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	if sonarr.host["authenticationMethod"] != "forms" || sonarr.host["username"] != "admin" || sonarr.host["password"] != pass {
		t.Errorf("sonarr login not set: %v", sonarr.host)
	}
	if got := sonarr.lists["/rootfolder"]; len(got) != 1 || got[0]["path"] != "/data/media/tv" {
		t.Errorf("sonarr root folders = %v", got)
	}
	if got := radarr.lists["/rootfolder"]; len(got) != 1 || got[0]["path"] != "/data/media/movies" {
		t.Errorf("radarr root folders = %v", got)
	}
	dc := sonarr.lists["/downloadclient"]
	if len(dc) != 1 {
		t.Fatalf("sonarr download clients = %v", dc)
	}
	if fieldValue(dc[0], "host") != "qbittorrent" || fieldValue(dc[0], "port") != float64(8080) ||
		fieldValue(dc[0], "password") != pass || fieldValue(dc[0], "tvCategory") != "tv" {
		t.Errorf("sonarr download client fields wrong: %v", dc[0])
	}
	if _, ok := dc[0]["presets"]; ok {
		t.Error("schema presets were posted back")
	}
	if got := fieldValue(radarr.lists["/downloadclient"][0], "movieCategory"); got != "movies" {
		t.Errorf("radarr category = %v", got)
	}
	n := radarr.lists["/notification"]
	if len(n) != 1 || fieldValue(n[0], "apiKey") != "jfkey" || fieldValue(n[0], "host") != "jellyfin" || n[0]["onDownload"] != true {
		t.Errorf("radarr notification = %v", n)
	}
	if _, ok := n[0]["onRename"]; ok {
		t.Error("onRename set although the schema does not support it")
	}
	pa := prowl.lists["/applications"]
	if len(pa) != 2 {
		t.Fatalf("prowlarr applications = %v", pa)
	}
	for _, app := range pa {
		if app["implementation"] == "Sonarr" {
			if fieldValue(app, "baseUrl") != "http://sonarr:8989" || fieldValue(app, "prowlarrUrl") != "http://prowlarr:9696" ||
				fieldValue(app, "apiKey") != secrets.Get("SONARR_API_KEY") {
				t.Errorf("prowlarr sonarr application wrong: %v", app)
			}
		}
	}
	if len(prowl.lists["/rootfolder"]) != 0 || len(prowl.lists["/downloadclient"]) != 0 {
		t.Error("prowlarr got a root folder or download client")
	}
	if !jf.wizardDone || jf.user != "admin" || jf.pass != pass {
		t.Errorf("jellyfin wizard: done=%v user=%q", jf.wizardDone, jf.user)
	}
	if strings.Join(jf.libraries, ",") != "Movies=/data/media/movies,Shows=/data/media/tv" {
		t.Errorf("jellyfin libraries = %v", jf.libraries)
	}

	// A second run must not change anything.
	before := []int{sonarr.writes, radarr.writes, prowl.writes, len(jf.libraries), len(jf.keys)}
	if err := Run(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	after := []int{sonarr.writes, radarr.writes, prowl.writes, len(jf.libraries), len(jf.keys)}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("second run was not idempotent: writes before %v, after %v", before, after)
			break
		}
	}
	if qbLogins != 2 {
		t.Errorf("qbittorrent logins = %d, want 2", qbLogins)
	}
}

func TestHealthyChecksBody(t *testing.T) {
	body := "Degraded"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
	defer srv.Close()
	apps, err := catalog.Resolve([]string{"jellyfin"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := NewCtx(stack.Default(t.TempDir()), nil, apps, io.Discard)
	ctx.BaseURLs = map[string]string{"jellyfin": srv.URL}
	if Healthy(ctx, apps[0]) {
		t.Error("a loading Jellyfin (200 Degraded) counts as ready")
	}
	body = "Healthy"
	if !Healthy(ctx, apps[0]) {
		t.Error("a healthy Jellyfin does not count as ready")
	}
}

func TestQBPasswordHash(t *testing.T) {
	// Reference value from Python's hashlib.pbkdf2_hmac('sha512', ..., 100000, 64).
	got, err := qbPasswordHash("adminadmin", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	want := "@ByteArray(MDEyMzQ1Njc4OWFiY2RlZg==:ZZS5EElovMoBVYDjMAaPI3BYalEsuNQNBCv4gjnI6KDYLC66zug9YBa5FIuP+3n/qFDM467FTFd4KkOdsg0Ugw==)"
	if got != want {
		t.Errorf("hash = %s\nwant   %s", got, want)
	}
}

func TestQBittorrentPrepareKeepsExistingConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := stack.Default(dir)
	apps, _ := catalog.Resolve([]string{"qbittorrent"})
	secrets, _ := stack.LoadSecrets(dir)
	secrets.EnsureAdmin()
	ctx := NewCtx(cfg, secrets, apps, io.Discard)

	if err := Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config", "qbittorrent", "qBittorrent", "qBittorrent.conf")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `WebUI\Password_PBKDF2="@ByteArray(`) || !strings.Contains(string(b), `WebUI\Username=admin`) ||
		!strings.Contains(string(b), `WebUI\HostHeaderValidation=false`) {
		t.Errorf("seeded config incomplete:\n%s", b)
	}
	if strings.Contains(string(b), secrets.Get(stack.AdminPassword)) {
		t.Error("config contains the plain password")
	}
	os.WriteFile(path, []byte("changed by the app"), 0o644)
	if err := Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "changed by the app" {
		t.Error("existing config was overwritten")
	}
}
