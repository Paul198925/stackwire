package wire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

// Fact is one number an app reports about itself, shown on the dashboard.
type Fact struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"` // "" for a count, "bytes/s" for a rate
}

// Facts asks a running app for its numbers. Apps without any return nil.
func Facts(c *Ctx, a catalog.App) ([]Fact, error) {
	switch a.Driver {
	case "arr", "prowlarr":
		return arrFacts(c, a)
	case "qbittorrent":
		return qbFacts(c, a)
	}
	return nil, nil
}

func arrFacts(c *Ctx, a catalog.App) ([]Fact, error) {
	if a.Arr == nil {
		return nil, nil
	}
	cl := newArrClient(c, a)
	var facts []Fact
	for _, n := range a.Arr.Counts {
		var raw json.RawMessage
		if err := cl.do("GET", n.Path, nil, &raw); err != nil {
			return facts, err
		}
		var value float64
		if n.Field == "" {
			// The count is the length of the returned list.
			var list []json.RawMessage
			if err := json.Unmarshal(raw, &list); err != nil {
				return facts, fmt.Errorf("%s: expected a list: %w", n.Path, err)
			}
			value = float64(len(list))
		} else {
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				return facts, fmt.Errorf("%s: expected an object: %w", n.Path, err)
			}
			v, ok := obj[n.Field].(float64)
			if !ok {
				return facts, fmt.Errorf("%s: no number in field %q", n.Path, n.Field)
			}
			value = v
		}
		facts = append(facts, Fact{Label: n.Label, Value: value})
	}
	return facts, nil
}

func qbFacts(c *Ctx, a catalog.App) ([]Fact, error) {
	base := c.BaseURL(a)
	form := url.Values{
		"username": {c.Secrets.Get(stack.AdminUser)},
		"password": {c.Secrets.Get(stack.AdminPassword)},
	}
	req, err := http.NewRequest("POST", base+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", base)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var cookies []string
	for _, ck := range resp.Cookies() {
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	if resp.StatusCode >= 300 || len(cookies) == 0 {
		return nil, fmt.Errorf("login failed (%s)", resp.Status)
	}
	header := map[string]string{"Cookie": strings.Join(cookies, "; "), "Referer": base}

	var transfer struct {
		Down float64 `json:"dl_info_speed"`
		Up   float64 `json:"up_info_speed"`
	}
	if err := doJSON(c.HTTP, "GET", base+"/api/v2/transfer/info", header, nil, &transfer); err != nil {
		return nil, err
	}
	var all, active []json.RawMessage
	if err := doJSON(c.HTTP, "GET", base+"/api/v2/torrents/info", header, nil, &all); err != nil {
		return nil, err
	}
	if err := doJSON(c.HTTP, "GET", base+"/api/v2/torrents/info?filter=downloading", header, nil, &active); err != nil {
		return nil, err
	}
	return []Fact{
		{Label: "Torrents", Value: float64(len(all))},
		{Label: "Downloading", Value: float64(len(active))},
		{Label: "Download speed", Value: transfer.Down, Unit: "bytes/s"},
		{Label: "Upload speed", Value: transfer.Up, Unit: "bytes/s"},
	}, nil
}
