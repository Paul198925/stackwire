package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"stackwire/internal/stack"
)

func setupBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	in := map[string]any{"password": "a-good.password!1", "apps": []string{"seerr"}, "timezone": "Europe/Berlin"}
	for k, v := range fields {
		in[k] = v
	}
	b, _ := json.Marshal(in)
	return string(b)
}

func TestSetup(t *testing.T) {
	c, dir := setup(t)
	if code, out := c.do("GET", "/api/setup", ""); code != 200 || out["pending"] != false {
		t.Fatalf("without a setup code: %d %v", code, out)
	}
	code, err := StartSetup(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st, out := c.do("GET", "/api/setup", ""); st != 200 || out["pending"] != true || out["apps"] != nil {
		t.Errorf("pending without code must not reveal anything: %d %v", st, out)
	}
	if st, _ := c.do("GET", "/api/setup?code=wrong", ""); st != http.StatusForbidden {
		t.Errorf("wrong code = %d", st)
	}
	if st, _ := c.do("POST", "/api/setup", setupBody(t, map[string]any{"code": "wrong"})); st != http.StatusForbidden {
		t.Errorf("setup with wrong code = %d", st)
	}
	st, out := c.do("GET", "/api/setup?code="+code, "")
	if st != 200 || out["password"] == "" || out["apps"] == nil {
		t.Fatalf("right code: %d %v", st, out)
	}
	if st, out := c.do("POST", "/api/setup", setupBody(t, map[string]any{"code": code, "password": "short"})); st != http.StatusBadRequest {
		t.Errorf("short password = %d %v", st, out)
	}
	if st, out := c.do("POST", "/api/setup", setupBody(t, map[string]any{"code": code, "password": "has$dollar-sign"})); st != http.StatusBadRequest {
		t.Errorf("password with $ = %d %v", st, out)
	}
	if st, out := c.do("POST", "/api/setup", setupBody(t, map[string]any{"code": code, "domain": "media.example.com"})); st != http.StatusBadRequest {
		t.Errorf("domain without email = %d %v", st, out)
	}

	if st, out := c.do("POST", "/api/setup", setupBody(t, map[string]any{"code": code})); st != 200 {
		t.Fatalf("setup = %d %v", st, out)
	}
	// The setup signed the browser in.
	if st, out := c.do("GET", "/api/state", ""); st != 200 {
		t.Errorf("state after setup = %d %v", st, out)
	}
	cfg, err := stack.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.Apps, "seerr") || !slices.Contains(cfg.Apps, "jellyfin") {
		t.Errorf("apps = %v, want seerr with jellyfin", cfg.Apps)
	}
	sec, _ := stack.LoadSecrets(dir)
	if sec.Get(stack.AdminPassword) != "a-good.password!1" {
		t.Error("password not saved")
	}
	if _, err := os.Stat(filepath.Join(dir, SetupFile)); !os.IsNotExist(err) {
		t.Error("setup code still there after the setup")
	}
	if st, _ := c.do("POST", "/api/setup", setupBody(t, map[string]any{"code": code})); st != http.StatusConflict {
		t.Errorf("second setup = %d, want 409", st)
	}
}

func TestSetupKeepsPasswordOnceAppsRan(t *testing.T) {
	c, dir := setup(t)
	code, _ := StartSetup(dir)
	c.srv.Containers = func(string) (map[string]string, error) { return map[string]string{"sonarr": "running"}, nil }
	if st, out := c.do("POST", "/api/setup", setupBody(t, map[string]any{"code": code})); st != http.StatusBadRequest {
		t.Errorf("setup with running apps = %d %v", st, out)
	}
}
