package wire

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

// seerr completes Seerr's setup: it signs in with the Jellyfin admin, turns
// on the Jellyfin libraries and connects Sonarr and Radarr, so requests made
// in Seerr are sent to them.
type seerr struct{}

func (seerr) Prepare(*Ctx, catalog.App) error { return nil }

type seerrClient struct {
	base string
	hc   *http.Client
}

func (s *seerrClient) do(method, path string, in, out any) error {
	return doJSON(s.hc, method, s.base+"/api/v1"+path, nil, in, out)
}

func (seerr) Wire(c *Ctx, a catalog.App) error {
	jf, ok := c.app("jellyfin")
	if !ok {
		return errors.New("needs Jellyfin")
	}
	// Seerr identifies the admin by a session cookie.
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	s := &seerrClient{base: c.BaseURL(a), hc: &http.Client{Timeout: c.HTTP.Timeout, Jar: jar}}
	user, pass := c.Secrets.Get(stack.AdminUser), c.Secrets.Get(stack.AdminPassword)

	const mediaServerJellyfin = 2
	var public struct {
		Initialized     bool `json:"initialized"`
		MediaServerType int  `json:"mediaServerType"`
	}
	if err := s.do("GET", "/settings/public", nil, &public); err != nil {
		return err
	}
	// A setup that stopped halfway has Jellyfin stored but is not
	// initialized. Seerr rejects the server details a second time, so they
	// are only sent while no media server is stored.
	connected := public.MediaServerType == mediaServerJellyfin
	login := map[string]any{"username": user, "password": pass}
	if !connected {
		// The first sign-in also stores where Jellyfin is and makes this
		// account the owner.
		login["hostname"], login["port"], login["useSsl"], login["urlBase"] = jf.ID, jf.Port, false, ""
		login["serverType"] = mediaServerJellyfin
		login["email"] = seerrEmail(c, user)
	}
	if err := s.do("POST", "/auth/jellyfin", login, nil); err != nil {
		return fmt.Errorf("sign-in with the Jellyfin admin failed: %w", err)
	}
	switch {
	case public.Initialized:
		c.logf(a, "setup already completed")
	case connected:
		c.logf(a, "already connected to Jellyfin, finishing the setup")
	default:
		c.logf(a, "connected to Jellyfin, owner account %s created", user)
	}

	if err := s.ensureLibraries(c, a); err != nil {
		return err
	}
	for _, target := range c.Apps {
		if target.Arr == nil || target.Arr.Seerr == "" {
			continue
		}
		if err := s.ensureArr(c, a, target); err != nil {
			return fmt.Errorf("%s: %w", target.ID, err)
		}
	}
	if !public.Initialized {
		if err := s.do("POST", "/settings/initialize", nil, nil); err != nil {
			return err
		}
		c.logf(a, "setup completed")
	}
	return nil
}

// seerrEmail is the address of the owner account. Seerr requires one; it is
// only used if the user later turns on email notifications.
func seerrEmail(c *Ctx, user string) string {
	if c.Cfg.Email != "" {
		return c.Cfg.Email
	}
	return user + "@stackwire.invalid"
}

func (s *seerrClient) ensureLibraries(c *Ctx, a catalog.App) error {
	var libs []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	// Sync reads the libraries from Jellyfin and keeps the enabled flags.
	if err := s.do("POST", "/settings/jellyfin/library/sync", nil, &libs); err != nil {
		return err
	}
	for _, l := range libs {
		if l.Enabled {
			c.logf(a, "library %s is enabled", l.Name)
			continue
		}
		if err := s.do("PUT", "/settings/jellyfin/library/"+url.PathEscape(l.ID), map[string]bool{"enabled": true}, nil); err != nil {
			return err
		}
		c.logf(a, "library %s enabled", l.Name)
	}
	return nil
}

// ensureArr connects Sonarr or Radarr. kind is "sonarr" or "radarr", which
// is both the API path and the shape of the settings.
func (s *seerrClient) ensureArr(c *Ctx, a, target catalog.App) error {
	kind := target.Arr.Seerr
	var existing []map[string]any
	if err := s.do("GET", "/settings/"+kind, nil, &existing); err != nil {
		return err
	}
	if len(existing) > 0 {
		c.logf(a, "%s connection exists", target.Name)
		return nil
	}
	conn := map[string]any{
		"hostname": target.ID,
		"port":     target.Port,
		"apiKey":   c.Secrets.Get(target.Arr.APIKeySecret),
		"useSsl":   false,
		"baseUrl":  "",
	}
	// The test call returns the quality profiles, which the settings need.
	var test struct {
		Profiles []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"profiles"`
	}
	if err := s.do("POST", "/settings/"+kind+"/test", conn, &test); err != nil {
		return err
	}
	if len(test.Profiles) == 0 {
		return errors.New("no quality profiles found")
	}
	// 1080p is the common choice; the user can change it in Seerr.
	profile := test.Profiles[0]
	for _, p := range test.Profiles {
		if p.Name == "HD-1080p" {
			profile = p
		}
	}
	conn["name"] = target.Name
	conn["activeProfileId"], conn["activeProfileName"] = profile.ID, profile.Name
	conn["activeDirectory"] = target.Arr.RootFolder
	conn["is4k"], conn["isDefault"] = false, true
	conn["syncEnabled"], conn["preventSearch"] = true, false
	conn["tags"] = []any{}
	if kind == "radarr" {
		conn["minimumAvailability"] = "released"
	} else {
		conn["seriesType"], conn["animeSeriesType"] = "standard", "anime"
		conn["enableSeasonFolders"] = true
		conn["animeTags"] = []any{}
	}
	if err := s.do("POST", "/settings/"+kind, conn, nil); err != nil {
		return err
	}
	c.logf(a, "%s connected (quality profile %s)", target.Name, profile.Name)
	return nil
}
