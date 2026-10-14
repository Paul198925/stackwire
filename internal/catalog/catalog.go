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
	Driver      string            `yaml:"driver"`
	Arr         *ArrSpec          `yaml:"arr"`
	Jellyfin    *JellyfinSpec     `yaml:"jellyfin"`
}

// ArrSpec configures the Servarr family (Sonarr, Radarr, Prowlarr).
type ArrSpec struct {
	APIVersion   string     `yaml:"apiVersion"`
	APIKeySecret string     `yaml:"apiKeySecret"`
	RootFolder   string     `yaml:"rootFolder"`
	Category     string     `yaml:"category"`    // download client category
	ProwlarrApp  string     `yaml:"prowlarrApp"` // Prowlarr application implementation
	Counts       []ArrCount `yaml:"counts"`      // numbers for the dashboard
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

// Resolve returns the apps with the given IDs, in the given order.
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
	return out, nil
}
