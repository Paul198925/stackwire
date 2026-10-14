package wire

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

const bazarrKeySecret = "BAZARR_API_KEY"

// bazarr seeds the login before the first start and then points Bazarr at
// Sonarr and Radarr, so it can fetch subtitles for what they import.
type bazarr struct{}

func (bazarr) Prepare(c *Ctx, a catalog.App) error {
	// The linuxserver image keeps the file at /config/config/config.yaml.
	path := filepath.Join(c.Cfg.Abs(c.Cfg.ConfigDir), a.ID, "config", "config.yaml")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Bazarr stores the password as an MD5 hex digest. Everything left out
	// here gets Bazarr's defaults.
	sum := md5.Sum([]byte(c.Secrets.Get(stack.AdminPassword)))
	conf := fmt.Sprintf("auth:\n  type: form\n  username: %s\n  password: %s\n  apikey: %s\n",
		strconv.Quote(c.Secrets.Get(stack.AdminUser)), hex.EncodeToString(sum[:]), c.Secrets.Get(bazarrKeySecret))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(conf), 0o644)
}

func (bazarr) Wire(c *Ctx, a catalog.App) error {
	base := c.BaseURL(a) + "/api/system/settings"
	header := map[string]string{"X-API-KEY": c.Secrets.Get(bazarrKeySecret)}
	var current map[string]map[string]any
	if err := doJSON(c.HTTP, "GET", base, header, nil, &current); err != nil {
		return fmt.Errorf("reading settings (if this Bazarr was set up by hand, set its API key to %s from %s): %w",
			bazarrKeySecret, stack.SecretsFile, err)
	}
	form := url.Values{}
	for _, target := range c.Apps {
		// Bazarr has one fixed section each for Sonarr and Radarr.
		if target.ID != "sonarr" && target.ID != "radarr" || target.Arr == nil {
			continue
		}
		want := map[string]string{
			"ip":     target.ID,
			"port":   strconv.Itoa(target.Port),
			"apikey": c.Secrets.Get(target.Arr.APIKeySecret),
		}
		same := fmt.Sprint(current["general"]["use_"+target.ID]) == "true"
		for k, v := range want {
			same = same && fmt.Sprint(current[target.ID][k]) == v
		}
		if same {
			c.logf(a, "connection to %s exists", target.Name)
			continue
		}
		form.Set("settings-general-use_"+target.ID, "true")
		for k, v := range want {
			form.Set("settings-"+target.ID+"-"+k, v)
		}
		form.Set("settings-"+target.ID+"-base_url", "/")
		form.Set("settings-"+target.ID+"-ssl", "false")
		c.logf(a, "connection to %s added", target.Name)
	}
	if len(form) == 0 {
		return nil
	}
	req, err := http.NewRequest("POST", base, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-API-KEY", header["X-API-KEY"])
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return fmt.Errorf("saving settings: %s: %s", resp.Status, body)
	}
	return nil
}
