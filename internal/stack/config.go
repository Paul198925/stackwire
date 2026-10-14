// Package stack holds the user's stack configuration and its secrets.
package stack

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const ConfigFile = "stackwire.yaml"

// Config is the content of stackwire.yaml.
type Config struct {
	Name      string         `yaml:"name"`
	Apps      []string       `yaml:"apps"`
	Domain    string         `yaml:"domain"` // empty: LAN mode, apps are reached by port
	Email     string         `yaml:"email"`  // Let's Encrypt contact, domain mode only
	DataDir   string         `yaml:"dataDir"`
	ConfigDir string         `yaml:"configDir"`
	Timezone  string         `yaml:"timezone"`
	PUID      int            `yaml:"puid"`
	PGID      int            `yaml:"pgid"`
	Ports     map[string]int `yaml:"ports,omitempty"` // host port overrides by app ID

	Dir string `yaml:"-"` // directory that contains stackwire.yaml
}

// Default returns the configuration `stackwire init` starts from.
func Default(dir string) *Config {
	uid, gid := os.Getuid(), os.Getgid()
	// Windows reports -1; root must not own the media files.
	if uid <= 0 {
		uid, gid = 1000, 1000
	}
	return &Config{
		Name:      "stackwire",
		Apps:      []string{"jellyfin", "sonarr", "radarr", "prowlarr", "qbittorrent"},
		DataDir:   "./data",
		ConfigDir: "./config",
		Timezone:  detectTimezone(),
		PUID:      uid,
		PGID:      gid,
		Dir:       dir,
	}
}

func Load(dir string) (*Config, error) {
	b, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no %s in %s (run `stackwire init` first)", ConfigFile, dir)
	}
	if err != nil {
		return nil, err
	}
	c := Default(dir)
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	c.Dir = dir
	return c, c.Validate()
}

func (c *Config) Save() error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.Dir, ConfigFile), b, 0o644)
}

var (
	domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	emailRe  = regexp.MustCompile(`^[^\s@{}]+@[^\s@{}]+$`)
)

func (c *Config) Validate() error {
	if !nameRe.MatchString(c.Name) {
		return fmt.Errorf("name %q must be lowercase letters, digits, - or _", c.Name)
	}
	if len(c.Apps) == 0 {
		return errors.New("no apps selected")
	}
	if c.Domain != "" && !domainRe.MatchString(c.Domain) {
		return fmt.Errorf("domain %q is not a valid hostname", c.Domain)
	}
	if c.Email != "" && !emailRe.MatchString(c.Email) {
		return fmt.Errorf("email %q is not valid", c.Email)
	}
	if c.DataDir == "" || c.ConfigDir == "" {
		return errors.New("dataDir and configDir must be set")
	}
	return nil
}

// HostPort is the port an app's web UI is published on.
func (c *Config) HostPort(appID string, containerPort int) int {
	if p := c.Ports[appID]; p != 0 {
		return p
	}
	return containerPort
}

// PublicURL is where a person reaches the app.
func (c *Config) PublicURL(appID, subdomain string, containerPort int) string {
	if c.Domain != "" {
		return fmt.Sprintf("https://%s.%s", subdomain, c.Domain)
	}
	return fmt.Sprintf("http://localhost:%d", c.HostPort(appID, containerPort))
}

// Abs resolves a path from the config against the stack directory.
func (c *Config) Abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

func detectTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	if p, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(p, "zoneinfo/"); i >= 0 {
			return p[i+len("zoneinfo/"):]
		}
	}
	return "Etc/UTC"
}
