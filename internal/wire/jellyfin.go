package wire

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

const jellyfinKeyName = "stackwire"

// jellyfin completes the first-run wizard, adds the libraries from the
// manifest and creates the API key the *arr apps use for library scans.
type jellyfin struct{}

func (jellyfin) Prepare(*Ctx, catalog.App) error { return nil }

type jfClient struct {
	base  string
	token string
	hc    *http.Client
}

func (j *jfClient) do(method, path string, in, out any) error {
	auth := `MediaBrowser Client="stackwire", Device="stackwire", DeviceId="stackwire-cli", Version="0.1.0"`
	if j.token != "" {
		auth += `, Token="` + j.token + `"`
	}
	// Jellyfin answers 503 for a moment while it switches from its startup
	// server to the real one.
	var err error
	for i := 0; i < 60; i++ {
		err = doJSON(j.hc, method, j.base+path, map[string]string{"Authorization": auth}, in, out)
		var he *httpError
		if !errors.As(err, &he) || he.status != http.StatusServiceUnavailable {
			return err
		}
		time.Sleep(2 * time.Second)
	}
	return err
}

func (jellyfin) Wire(c *Ctx, a catalog.App) error {
	j := &jfClient{base: c.BaseURL(a), hc: c.HTTP}
	user, pass := c.Secrets.Get(stack.AdminUser), c.Secrets.Get(stack.AdminPassword)

	var info struct {
		StartupWizardCompleted bool
	}
	if err := j.do("GET", "/System/Info/Public", nil, &info); err != nil {
		return err
	}
	if info.StartupWizardCompleted {
		c.logf(a, "setup wizard already completed")
	} else {
		if err := j.runWizard(user, pass); err != nil {
			return fmt.Errorf("setup wizard: %w", err)
		}
		c.logf(a, "setup wizard completed, admin user %s created", user)
	}

	var login struct {
		AccessToken string
	}
	if err := j.do("POST", "/Users/AuthenticateByName", map[string]string{"Username": user, "Pw": pass}, &login); err != nil {
		return fmt.Errorf("login with the stack's admin account failed; if this Jellyfin was set up by hand, "+
			"create an administrator with the values in %s: %w", stack.SecretsFile, err)
	}
	j.token = login.AccessToken

	if a.Jellyfin != nil {
		if err := j.ensureLibraries(c, a); err != nil {
			return err
		}
	}
	key, err := j.ensureAPIKey()
	if err != nil {
		return err
	}
	c.jellyfinKey = key
	return nil
}

func (j *jfClient) runWizard(user, pass string) error {
	conf := map[string]string{"UICulture": "en-US", "MetadataCountryCode": "US", "PreferredMetadataLanguage": "en"}
	if err := j.do("POST", "/Startup/Configuration", conf, nil); err != nil {
		return err
	}
	// Reading the user first is required: it creates the initial account
	// that the following POST renames and gives a password.
	if err := j.do("GET", "/Startup/User", nil, nil); err != nil {
		return err
	}
	if err := j.do("POST", "/Startup/User", map[string]string{"Name": user, "Password": pass}, nil); err != nil {
		return err
	}
	remote := map[string]bool{"EnableRemoteAccess": true, "EnableAutomaticPortMapping": false}
	if err := j.do("POST", "/Startup/RemoteAccess", remote, nil); err != nil {
		return err
	}
	return j.do("POST", "/Startup/Complete", nil, nil)
}

func (j *jfClient) ensureLibraries(c *Ctx, a catalog.App) error {
	var current []struct {
		Name string
	}
	if err := j.do("GET", "/Library/VirtualFolders", nil, &current); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, l := range current {
		have[l.Name] = true
	}
	for _, lib := range a.Jellyfin.Libraries {
		if have[lib.Name] {
			c.logf(a, "library %s exists", lib.Name)
			continue
		}
		q := url.Values{
			"name":           {lib.Name},
			"collectionType": {lib.Type},
			"paths":          {lib.Path},
			"refreshLibrary": {"false"},
		}
		// An empty LibraryOptions object keeps the server's defaults.
		body := map[string]any{"LibraryOptions": map[string]any{}}
		if err := j.do("POST", "/Library/VirtualFolders?"+q.Encode(), body, nil); err != nil {
			return err
		}
		c.logf(a, "library %s added (%s)", lib.Name, lib.Path)
	}
	return nil
}

func (j *jfClient) ensureAPIKey() (string, error) {
	find := func() (string, error) {
		var keys struct {
			Items []struct {
				AccessToken string
				AppName     string
			}
		}
		if err := j.do("GET", "/Auth/Keys", nil, &keys); err != nil {
			return "", err
		}
		for _, k := range keys.Items {
			if k.AppName == jellyfinKeyName {
				return k.AccessToken, nil
			}
		}
		return "", nil
	}
	key, err := find()
	if err != nil || key != "" {
		return key, err
	}
	if err := j.do("POST", "/Auth/Keys?app="+jellyfinKeyName, nil, nil); err != nil {
		return "", err
	}
	key, err = find()
	if err == nil && key == "" {
		err = errors.New("API key was created but is not listed")
	}
	return key, err
}
