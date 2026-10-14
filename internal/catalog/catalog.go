// Package catalog holds the app manifests that ship with the binary.
package catalog

import (
	"embed"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

//go:embed apps/*.yaml
var appFS embed.FS

// App is one installable service. Everything the renderer needs is declared
// here; the Driver names the Go code that configures the app after it starts.
type App struct {
	ID          string            `yaml:"id"`
	Name        string            `yaml:"name"`
	Category    string            `yaml:"category"`
	Description string            `yaml:"description"`
	Image       string            `yaml:"image"`
	Port        int               `yaml:"port"`       // web UI port inside the container
	Subdomain   string            `yaml:"subdomain"`  // defaults to ID
	Health      string            `yaml:"health"`     // path that returns 200 once the app is ready
	HealthBody  string            `yaml:"healthBody"` // if set, the response must also be exactly this text
	Env         map[string]string `yaml:"env"`
	Secrets     map[string]string `yaml:"secrets"`    // .env name -> kind (hex32, password)
	Volumes     []string          `yaml:"volumes"`    // "config:/path" or "data:/path"
	ExtraPorts  []string          `yaml:"extraPorts"` // published as-is
	DataDirs    []string          `yaml:"dataDirs"`   // created below the data directory
	Requires    []string          `yaml:"requires"`   // apps that must be installed as well
	Internal    bool              `yaml:"internal"`   // used by other apps only: never published or routed
	RunAsUser   bool              `yaml:"runAsUser"`  // start the container as PUID:PGID (images without PUID support)
	Todo        string            `yaml:"todo"`       // what only the user can decide, shown after setup
	Driver      string            `yaml:"driver"`
	Arr         *ArrSpec          `yaml:"arr"`
	Jellyfin    *JellyfinSpec     `yaml:"jellyfin"`
}

// ArrSpec configures the Servarr family (Sonarr, Radarr, Prowlarr).
type ArrSpec struct {
	APIVersion   string `yaml:"apiVersion"`
	APIKeySecret string `yaml:"apiKeySecret"`
	RootFolder   string `yaml:"rootFolder"`
	// RootFolderName is set for apps whose root folders need a name and
	// default profiles (Lidarr).
	RootFolderName string `yaml:"rootFolderName"`
	// Seerr is "sonarr" or "radarr" if Seerr can send requests to this app.
	Seerr       string     `yaml:"seerr"`
	Category    string     `yaml:"category"`    // download client category
	ProwlarrApp string     `yaml:"prowlarrApp"` // Prowlarr application implementation
	Counts      []ArrCount `yaml:"counts"`      // numbers for the dashboard
}

// ArrCount is a number read from a Servarr API endpoint.
type ArrCount struct {
	Label string `yaml:"label"`
	Path  string `yaml:"path"`
	// Field names the number in the response object. Empty: the response is
	// a list and its length is the number.
	Field string `yaml:"field"`
}

type JellyfinSpec struct {
	Libraries []JellyfinLibrary `yaml:"libraries"`
}

type JellyfinLibrary struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	Path string `yaml:"path"`
	// Requires names the app that fills this library; without it the
	// library is not created.
	Requires string `yaml:"requires"`
}

// All returns every app in the catalog, sorted by ID.
func All() ([]App, error) {
	entries, err := appFS.ReadDir("apps")
	if err != nil {
		return nil, err
	}
	var apps []App
	for _, e := range entries {
		b, err := appFS.ReadFile("apps/" + e.Name())
		if err != nil {
			return nil, err
		}
		var a App
		if err := yaml.Unmarshal(b, &a); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if a.ID == "" || a.Image == "" || a.Port == 0 {
			return nil, fmt.Errorf("%s: id, image and port are required", e.Name())
		}
		if a.Subdomain == "" {
			a.Subdomain = a.ID
		}
		apps = append(apps, a)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	return apps, nil
}

// Get returns one app from the catalog.
func Get(id string) (App, error) {
	all, err := All()
	if err != nil {
		return App{}, err
	}
	for _, a := range all {
		if a.ID == id {
			return a, nil
		}
	}
	return App{}, fmt.Errorf("unknown app %q (run `stackwire apps` for the catalog)", id)
}

// Resolve returns the apps with the given IDs, in the given order. It fails
// if an app is selected without the apps it needs.
func Resolve(ids []string) ([]App, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	byID := map[string]App{}
	for _, a := range all {
		byID[a.ID] = a
	}
	var out []App
	seen := map[string]bool{}
	for _, id := range ids {
		a, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown app %q (run `stackwire apps` for the catalog)", id)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, a)
		}
	}
	for _, a := range out {
		for _, req := range a.Requires {
			if !seen[req] {
				return nil, fmt.Errorf("%s needs %s, which is not selected", a.ID, req)
			}
		}
	}
	return out, nil
}

// WithRequirements adds the apps that the given ones need, directly or
// through another app, and returns the IDs in a stable order.
func WithRequirements(ids []string) ([]string, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	byID := map[string]App{}
	for _, a := range all {
		byID[a.ID] = a
	}
	seen := map[string]bool{}
	var out []string
	var add func(id string) error
	add = func(id string) error {
		a, ok := byID[id]
		if !ok {
			return fmt.Errorf("unknown app %q", id)
		}
		if seen[id] {
			return nil
		}
		seen[id] = true
		for _, req := range a.Requires {
			if err := add(req); err != nil {
				return err
			}
		}
		out = append(out, id)
		return nil
	}
	for _, id := range ids {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	return out, nil
}
