package wire

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

const sabnzbdKeySecret = "SABNZBD_API_KEY"

// sabnzbd configures the usenet download client: login, folders, one
// category per media type and the API key the *arr apps use. The news
// server is the user's own and has to be added by hand.
type sabnzbd struct{}

func (sabnzbd) Prepare(*Ctx, catalog.App) error { return nil }

type sabClient struct {
	base, key string
	c         *Ctx
}

// set changes one option through SABnzbd's API.
func (s *sabClient) call(params url.Values, out any) error {
	params.Set("apikey", s.key)
	params.Set("output", "json")
	return doJSON(s.c.HTTP, "GET", s.base+"/api?"+params.Encode(), nil, nil, out)
}

func (sabnzbd) Wire(c *Ctx, a catalog.App) error {
	key, err := sabnzbdKey(c, a)
	if err != nil {
		return err
	}
	s := &sabClient{base: c.BaseURL(a), key: key, c: c}

	var cfg struct {
		Config struct {
			Misc       map[string]any   `json:"misc"`
			Categories []map[string]any `json:"categories"`
		} `json:"config"`
	}
	if err := s.call(url.Values{"mode": {"get_config"}}, &cfg); err != nil {
		return err
	}
	misc := cfg.Config.Misc
	user := c.Secrets.Get(stack.AdminUser)
	want := map[string]string{
		"download_dir": "/data/usenet/incomplete",
		"complete_dir": "/data/usenet/complete",
	}
	// A fresh SABnzbd has no login. The password is not returned by the
	// API, so a set username is taken as "login configured".
	if fmt.Sprint(misc["username"]) == "" {
		want["username"] = user
		want["password"] = c.Secrets.Get(stack.AdminPassword)
	}
	changed := false
	for k, v := range want {
		if fmt.Sprint(misc[k]) == v {
			continue
		}
		if err := s.call(url.Values{"mode": {"set_config"}, "section": {"misc"}, "keyword": {k}, "value": {v}}, nil); err != nil {
			return err
		}
		changed = true
	}
	if _, ok := want["username"]; ok {
		c.logf(a, "login enabled for user %s", user)
	} else if !changed {
		c.logf(a, "login and folders already configured")
	}

	have := map[string]bool{}
	for _, cat := range cfg.Config.Categories {
		have[fmt.Sprint(cat["name"])] = true
	}
	for _, target := range c.Apps {
		if target.Arr == nil || target.Arr.Category == "" || have[target.Arr.Category] {
			continue
		}
		name := target.Arr.Category
		if err := s.call(url.Values{"mode": {"set_config"}, "section": {"categories"}, "name": {name}, "dir": {name}}, nil); err != nil {
			return err
		}
		have[name] = true
		c.logf(a, "category %s added", name)
	}
	return nil
}

// sabnzbdKey returns SABnzbd's API key. SABnzbd generates it on its first
// start and writes it to its config file, which lives in the stack folder.
func sabnzbdKey(c *Ctx, a catalog.App) (string, error) {
	path := filepath.Join(c.Cfg.Abs(c.Cfg.ConfigDir), a.ID, "sabnzbd.ini")
	var key string
	// The file can lag behind the health check by a moment.
	for i := 0; i < 15 && key == ""; i++ {
		if i > 0 {
			time.Sleep(time.Second)
		}
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), "=")
			if ok && strings.TrimSpace(k) == "api_key" {
				key = strings.TrimSpace(v)
				break
			}
		}
		f.Close()
	}
	if key == "" {
		return "", fmt.Errorf("no api_key in %s", path)
	}
	// The *arr apps read it from the secrets, like every other key.
	if c.Secrets.Get(sabnzbdKeySecret) != key {
		c.Secrets.Set(sabnzbdKeySecret, key)
		if err := c.Secrets.Save(); err != nil {
			return "", err
		}
	}
	return key, nil
}

func sabnzbdFacts(c *Ctx, a catalog.App) ([]Fact, error) {
	key := c.Secrets.Get(sabnzbdKeySecret)
	if key == "" {
		return nil, nil
	}
	s := &sabClient{base: c.BaseURL(a), key: key, c: c}
	var q struct {
		Queue struct {
			Slots    int    `json:"noofslots"`
			KBPerSec string `json:"kbpersec"`
		} `json:"queue"`
	}
	if err := s.call(url.Values{"mode": {"queue"}, "limit": {"1"}}, &q); err != nil {
		return nil, err
	}
	var kb float64
	fmt.Sscan(q.Queue.KBPerSec, &kb)
	return []Fact{
		{Label: "In queue", Value: float64(q.Queue.Slots)},
		{Label: "Download speed", Value: kb * 1024, Unit: "bytes/s"},
	}, nil
}
