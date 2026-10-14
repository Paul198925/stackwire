// Package wire configures the apps after they start: logins, folders and the
// connections between them. Every step checks the current state first, so
// running it again changes nothing.
package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

// Ctx is shared by all drivers during one run.
type Ctx struct {
	Cfg     *stack.Config
	Secrets *stack.Secrets
	Apps    []catalog.App
	HTTP    *http.Client
	Out     io.Writer

	// BaseURLs overrides where an app is reached, by app ID. Used in tests.
	BaseURLs map[string]string

	jellyfinKey string // API key the *arr apps use to trigger library scans
}

// Driver configures one kind of app.
type Driver interface {
	// Prepare runs before the containers start, e.g. to seed config files.
	Prepare(c *Ctx, a catalog.App) error
	// Wire runs once every app answers its health check.
	Wire(c *Ctx, a catalog.App) error
}

// Drivers run in this order: apps that others connect to come first.
var drivers = []struct {
	name string
	d    Driver
}{
	{"qbittorrent", qbittorrent{}},
	{"jellyfin", jellyfin{}},
	{"arr", arr{}},
	{"prowlarr", prowlarr{}},
	{"bazarr", bazarr{}},
	{"seerr", seerr{}},
}

func NewCtx(cfg *stack.Config, secrets *stack.Secrets, apps []catalog.App, out io.Writer) *Ctx {
	return &Ctx{Cfg: cfg, Secrets: secrets, Apps: apps, Out: out, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// BaseURL is where the CLI reaches the app from the host.
func (c *Ctx) BaseURL(a catalog.App) string {
	if u := c.BaseURLs[a.ID]; u != "" {
		return u
	}
	return fmt.Sprintf("http://127.0.0.1:%d", c.Cfg.HostPort(a.ID, a.Port))
}

func (c *Ctx) app(id string) (catalog.App, bool) {
	for _, a := range c.Apps {
		if a.ID == id {
			return a, true
		}
	}
	return catalog.App{}, false
}

func (c *Ctx) logf(a catalog.App, format string, args ...any) {
	fmt.Fprintf(c.Out, "  %-12s %s\n", a.ID, fmt.Sprintf(format, args...))
}

func (c *Ctx) ordered() []struct {
	a catalog.App
	d Driver
} {
	rank := map[string]int{}
	byName := map[string]Driver{}
	for i, e := range drivers {
		rank[e.name], byName[e.name] = i, e.d
	}
	var out []struct {
		a catalog.App
		d Driver
	}
	for _, a := range c.Apps {
		if d, ok := byName[a.Driver]; ok {
			out = append(out, struct {
				a catalog.App
				d Driver
			}{a, d})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].a.Driver] < rank[out[j].a.Driver] })
	return out
}

// Prepare runs every driver's pre-start step.
func Prepare(c *Ctx) error {
	for _, e := range c.ordered() {
		if err := e.d.Prepare(c, e.a); err != nil {
			return fmt.Errorf("%s: %w", e.a.ID, err)
		}
	}
	return nil
}

// Run configures all apps. They must be healthy; see Wait.
func Run(c *Ctx) error {
	for _, e := range c.ordered() {
		if err := e.d.Wire(c, e.a); err != nil {
			return fmt.Errorf("%s: %w", e.a.ID, err)
		}
	}
	return nil
}

// Healthy reports whether the app answers its health check right now.
func Healthy(c *Ctx, a catalog.App) bool {
	resp, err := c.HTTP.Get(c.BaseURL(a) + a.Health)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	if a.HealthBody == "" {
		return true
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return strings.TrimSpace(string(b)) == a.HealthBody
}

// Wait blocks until every app is healthy or the timeout expires.
func Wait(c *Ctx, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, a := range c.Apps {
		for !Healthy(c, a) {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not become ready at %s%s within %s (check `docker compose logs %s`)",
					a.ID, c.BaseURL(a), a.Health, timeout, a.ID)
			}
			time.Sleep(2 * time.Second)
		}
		c.logf(a, "ready")
	}
	return nil
}

// doJSON sends a request with an optional JSON body and decodes a JSON
// response into out when out is not nil. Status codes from 400 are errors.
func doJSON(hc *http.Client, method, url string, header map[string]string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		msg := string(b)
		if len(msg) > 400 {
			msg = msg[:400] + "..."
		}
		return &httpError{resp.StatusCode, fmt.Sprintf("%s %s: %s: %s", method, url, resp.Status, msg)}
	}
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("%s %s: decoding response: %w", method, url, err)
		}
	}
	return nil
}

// httpError is returned by doJSON for status codes from 400.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }
