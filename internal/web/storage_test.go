package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stackwire/internal/stack"
	"stackwire/internal/storage"
)

func TestStorageAPI(t *testing.T) {
	c, dir := setup(t)
	login(t, c, dir)
	var checked *stack.Storage
	c.srv.CheckStorage = func(_ *stack.Config, _ *stack.Secrets, st *stack.Storage, pw string) (storage.CheckResult, error) {
		checked = st
		return storage.CheckResult{Total: 100, Free: 40}, nil
	}
	usb := t.TempDir()
	body := `{"storage":[{"id":"usb","type":"folder","path":` + quote(usb) + `},
		{"id":"nas","name":"NAS","type":"smb","server":"nas.local","share":"media","user":"paul"}],
		"areas":{"media":{"storage":"nas","folder":"stackwire"},"photos":{"storage":"usb","folder":"photos"}},
		"passwords":{"nas":"s3cret-pass"}}`
	if code, out := c.do("PUT", "/api/storage", body); code != 200 {
		t.Fatalf("save = %d %v", code, out)
	}
	cfg, _ := stack.Load(dir)
	if len(cfg.Storage) != 2 || cfg.Areas["media"].Storage != "nas" {
		t.Errorf("config = %+v %+v", cfg.Storage, cfg.Areas)
	}
	sec, _ := stack.LoadSecrets(dir)
	if sec.Get(stack.StoragePasswordKey("nas")) != "s3cret-pass" {
		t.Error("password not stored")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, stack.ConfigFile)); strings.Contains(string(b), "s3cret") {
		t.Error("password written to stackwire.yaml")
	}

	code, out := c.do("GET", "/api/storage", "")
	if code != 200 || strings.Contains(toJSON(out), "s3cret") {
		t.Fatalf("get = %d, leaks password: %v", code, strings.Contains(toJSON(out), "s3cret"))
	}
	if !strings.Contains(toJSON(out), `"hasPassword":true`) || !strings.Contains(toJSON(out), `"present":true`) {
		t.Errorf("get = %s", toJSON(out))
	}

	// A storage that still holds an area cannot be removed.
	if code, out := c.do("PUT", "/api/storage", `{"storage":[{"id":"usb","type":"folder","path":`+quote(usb)+`}],
		"areas":{"media":{"storage":"nas","folder":"stackwire"}}}`); code != http.StatusBadRequest {
		t.Errorf("remove used storage = %d %v", code, out)
	}
	// Removing it together with its area works and forgets the password.
	if code, out := c.do("PUT", "/api/storage", `{"storage":[{"id":"usb","type":"folder","path":`+quote(usb)+`}],"areas":{}}`); code != 200 {
		t.Fatalf("remove = %d %v", code, out)
	}
	sec, _ = stack.LoadSecrets(dir)
	if sec.Get(stack.StoragePasswordKey("nas")) != "" {
		t.Error("password of a removed share kept")
	}
	if code, out := c.do("PUT", "/api/storage", `{"storage":[{"id":"nas","type":"smb","server":"nas","share":"m","user":"u"}],"passwords":{"nas":"a,b"}}`); code != http.StatusBadRequest {
		t.Errorf("comma in password = %d %v", code, out)
	}

	if code, out := c.do("POST", "/api/storage/check", `{"storage":{"type":"folder","path":`+quote(filepath.Join(usb, "gone"))+`}}`); code != http.StatusBadRequest {
		t.Errorf("check of a missing path = %d %v", code, out)
	}
	if code, out := c.do("POST", "/api/storage/check", `{"storage":{"type":"folder","path":`+quote(usb)+`}}`); code != 200 || out["free"] != float64(40) || checked == nil {
		t.Errorf("check = %d %v", code, out)
	}
}
