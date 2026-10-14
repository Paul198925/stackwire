package web

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"stackwire/internal/ops"
	"stackwire/internal/stack"
)

type client struct {
	t    *testing.T
	base string
	hc   *http.Client
	csrf bool
	srv  *Server
}

func (c *client) do(method, path, body string) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if c.csrf {
		req.Header.Set(csrfHeader, "1")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func setup(t *testing.T) (*client, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := stack.Default(dir)
	cfg.Apps = nil
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	srv := New(dir)
	srv.Containers = func(string) (map[string]string, error) { return map[string]string{}, nil }
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: ts.URL, hc: &http.Client{Jar: jar}, csrf: true, srv: srv}, dir
}

func login(t *testing.T, c *client, dir string) {
	t.Helper()
	st, err := ops.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"user": "admin", "password": st.Secrets.Get(stack.AdminPassword)})
	if code, out := c.do("POST", "/api/login", string(body)); code != 200 {
		t.Fatalf("login = %d %v", code, out)
	}
}

func TestAPIRequiresLogin(t *testing.T) {
	c, _ := setup(t)
	for _, r := range [][2]string{
		{"GET", "/api/state"}, {"GET", "/api/dashboard"}, {"PUT", "/api/config"}, {"POST", "/api/password"}, {"POST", "/api/apply"},
		{"POST", "/api/stop"}, {"POST", "/api/apps/sonarr/install"}, {"POST", "/api/apps/sonarr/remove"},
		{"POST", "/api/apps/sonarr/restart"}, {"GET", "/api/apps/sonarr/logs"},
	} {
		if code, _ := c.do(r[0], r[1], "{}"); code != http.StatusUnauthorized {
			t.Errorf("%s %s without login = %d, want 401", r[0], r[1], code)
		}
	}
	if code, _ := c.do("POST", "/api/login", `{"user":"admin","password":"wrong"}`); code != http.StatusUnauthorized {
		t.Errorf("login with wrong password = %d", code)
	}
}

func TestChangesNeedTheCSRFHeader(t *testing.T) {
	c, dir := setup(t)
	login(t, c, dir)
	c.csrf = false
	if code, _ := c.do("POST", "/api/apps/sonarr/install", ""); code != http.StatusForbidden {
		t.Errorf("POST without header = %d, want 403", code)
	}
	if code, _ := c.do("GET", "/api/state", ""); code != 200 {
		t.Errorf("GET without header = %d, want 200", code)
	}
}

func TestSelectAppsAndSettings(t *testing.T) {
	c, dir := setup(t)
	login(t, c, dir)

	_, st := c.do("GET", "/api/state", "")
	if st["pending"] != false || len(st["apps"].([]any)) < 5 {
		t.Fatalf("fresh state = %v", st)
	}
	if code, _ := c.do("POST", "/api/apps/nope/install", ""); code != http.StatusNotFound {
		t.Errorf("install of unknown app = %d", code)
	}
	for _, id := range []string{"sonarr", "jellyfin", "sonarr"} {
		if code, out := c.do("POST", "/api/apps/"+id+"/install", ""); code != 200 {
			t.Fatalf("install %s = %d %v", id, code, out)
		}
	}
	c.do("POST", "/api/apps/jellyfin/remove", "")
	cfg, _ := stack.Load(dir)
	if strings.Join(cfg.Apps, ",") != "sonarr" {
		t.Errorf("apps = %v, want [sonarr]", cfg.Apps)
	}
	if _, st := c.do("GET", "/api/state", ""); st["pending"] != true {
		t.Error("a selected app without container is not reported as pending")
	}

	code, out := c.do("PUT", "/api/config", `{"domain":"Media.Example.com","email":"a@b.c","timezone":"Europe/Berlin","dataDir":"./data","ports":{"sonarr":8989,"qbittorrent":8081}}`)
	if code != 200 {
		t.Fatalf("save settings = %d %v", code, out)
	}
	cfg, _ = stack.Load(dir)
	if cfg.Domain != "media.example.com" || cfg.Timezone != "Europe/Berlin" || len(cfg.Ports) != 1 || cfg.Ports["qbittorrent"] != 8081 {
		t.Errorf("saved config = %+v", cfg)
	}
	for name, body := range map[string]string{
		"bad domain":     `{"domain":"not a domain","timezone":"Etc/UTC","dataDir":"./data"}`,
		"bad timezone":   `{"timezone":"x; rm -rf","dataDir":"./data"}`,
		"port collision": `{"timezone":"Etc/UTC","dataDir":"./data","ports":{"sonarr":7878}}`,
		"port range":     `{"timezone":"Etc/UTC","dataDir":"./data","ports":{"sonarr":70000}}`,
	} {
		if code, _ := c.do("PUT", "/api/config", body); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	if cfg2, _ := stack.Load(dir); cfg2.Domain != "media.example.com" {
		t.Error("a rejected change was saved")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{
		"0B": 0, "512B": 512, "1.5kB": 1500, "2MB": 2000000, "123.4MiB": 129394278,
		" 1.5GiB ": 1610612736, "7.6GiB": 8160437862, "garbage": 0, "": 0, "12XB": 0,
	} {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestLoginIsThrottled(t *testing.T) {
	c, dir := setup(t)
	st, _ := ops.Load(dir)
	good, _ := json.Marshal(map[string]string{"user": "admin", "password": st.Secrets.Get(stack.AdminPassword)})
	// Failures recorded directly; going through the handler would sleep a second each.
	srv := c.srv
	srv.mu.Lock()
	for i := 0; i < maxLoginFailures; i++ {
		srv.failed = append(srv.failed, time.Now())
	}
	srv.mu.Unlock()
	if code, _ := c.do("POST", "/api/login", string(good)); code != http.StatusTooManyRequests {
		t.Errorf("login after %d failures = %d, want 429", maxLoginFailures, code)
	}
	srv.mu.Lock()
	for i := range srv.failed {
		srv.failed[i] = time.Now().Add(-2 * time.Minute)
	}
	srv.mu.Unlock()
	if code, _ := c.do("POST", "/api/login", string(good)); code != 200 {
		t.Errorf("login after the failures expired = %d, want 200", code)
	}
}

func TestInstallAddsRequirementsAndRemoveKeepsThem(t *testing.T) {
	c, dir := setup(t)
	login(t, c, dir)
	if code, out := c.do("POST", "/api/apps/seerr/install", ""); code != 200 {
		t.Fatalf("install seerr = %d %v", code, out)
	}
	cfg, _ := stack.Load(dir)
	if strings.Join(cfg.Apps, ",") != "jellyfin,seerr" {
		t.Errorf("apps = %v, want jellyfin installed along with seerr", cfg.Apps)
	}
	code, out := c.do("POST", "/api/apps/jellyfin/remove", "")
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "seerr needs jellyfin") {
		t.Errorf("removing a required app = %d %v", code, out)
	}
	// Restart and logs look the app up on its own, without its requirements.
	if code, out := c.do("GET", "/api/apps/nope/logs", ""); code != http.StatusNotFound {
		t.Errorf("logs of unknown app = %d %v", code, out)
	}
	if code, out := c.do("GET", "/api/apps/seerr/logs", ""); code == http.StatusNotFound {
		t.Errorf("logs of seerr = %d %v", code, out)
	}
	c.do("POST", "/api/apps/seerr/remove", "")
	if code, _ := c.do("POST", "/api/apps/jellyfin/remove", ""); code != 200 {
		t.Errorf("removing jellyfin after seerr = %d", code)
	}
}

func TestPasswordAndLogout(t *testing.T) {
	c, dir := setup(t)
	login(t, c, dir)
	st, _ := ops.Load(dir)
	if _, out := c.do("POST", "/api/password", ""); out["password"] != st.Secrets.Get(stack.AdminPassword) {
		t.Error("password endpoint returned something else")
	}
	c.do("POST", "/api/logout", "")
	if code, _ := c.do("GET", "/api/state", ""); code != http.StatusUnauthorized {
		t.Errorf("state after logout = %d", code)
	}
}
