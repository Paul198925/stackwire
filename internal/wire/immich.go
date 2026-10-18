package wire

import (
	"fmt"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

const (
	immichEmailSecret = "IMMICH_ADMIN_EMAIL"
	immichKeySecret   = "IMMICH_API_KEY"
)

// immich creates the admin account, which a fresh Immich otherwise hands to
// whoever opens it first, and an API key for the dashboard.
type immich struct{}

func (immich) Prepare(*Ctx, catalog.App) error { return nil }

func (immich) Wire(c *Ctx, a catalog.App) error {
	base := c.BaseURL(a) + "/api"
	pass := c.Secrets.Get(stack.AdminPassword)
	// Immich signs in with an email address. The one chosen at setup is
	// kept, so a later change of the stack's email does not lock anyone out.
	email := c.Secrets.Get(immichEmailSecret)
	if email == "" {
		email = adminEmail(c)
		c.Secrets.Set(immichEmailSecret, email)
		if err := c.Secrets.Save(); err != nil {
			return err
		}
	}

	var config struct {
		IsInitialized bool `json:"isInitialized"`
	}
	if err := doJSON(c.HTTP, "GET", base+"/server/config", nil, nil, &config); err != nil {
		return err
	}
	if config.IsInitialized {
		c.logf(a, "admin account already exists; sign in with %s", email)
	} else {
		signup := map[string]string{"email": email, "password": pass, "name": c.Secrets.Get(stack.AdminUser)}
		if err := doJSON(c.HTTP, "POST", base+"/auth/admin-sign-up", nil, signup, nil); err != nil {
			return err
		}
		c.logf(a, "admin account created; sign in with %s", email)
	}

	// A working key from an earlier run is kept.
	if key := c.Secrets.Get(immichKeySecret); key != "" {
		if doJSON(c.HTTP, "GET", base+"/server/statistics", map[string]string{"x-api-key": key}, nil, nil) == nil {
			c.logf(a, "API key works")
			return nil
		}
	}
	var login struct {
		AccessToken string `json:"accessToken"`
	}
	if err := doJSON(c.HTTP, "POST", base+"/auth/login", nil, map[string]string{"email": email, "password": pass}, &login); err != nil {
		return fmt.Errorf("sign-in as %s failed; if this Immich was set up by hand, set %s in %s to its admin's email: %w",
			email, immichEmailSecret, stack.SecretsFile, err)
	}
	var created struct {
		Secret string `json:"secret"`
	}
	auth := map[string]string{"Authorization": "Bearer " + login.AccessToken}
	body := map[string]any{"name": "stackwire", "permissions": []string{"server.statistics"}}
	if err := doJSON(c.HTTP, "POST", base+"/api-keys", auth, body, &created); err != nil {
		return err
	}
	c.Secrets.Set(immichKeySecret, created.Secret)
	if err := c.Secrets.Save(); err != nil {
		return err
	}
	c.logf(a, "API key for the dashboard created")
	return nil
}

// adminEmail is the address used where an app needs one for its admin.
func adminEmail(c *Ctx) string {
	if c.Cfg.Email != "" {
		return c.Cfg.Email
	}
	return c.Secrets.Get(stack.AdminUser) + "@stackwire.invalid"
}

func immichFacts(c *Ctx, a catalog.App) ([]Fact, error) {
	key := c.Secrets.Get(immichKeySecret)
	if key == "" {
		return nil, nil
	}
	var st struct {
		Photos int     `json:"photos"`
		Videos int     `json:"videos"`
		Usage  float64 `json:"usage"`
	}
	if err := doJSON(c.HTTP, "GET", c.BaseURL(a)+"/api/server/statistics", map[string]string{"x-api-key": key}, nil, &st); err != nil {
		return nil, err
	}
	return []Fact{
		{Label: "Photos", Value: float64(st.Photos)},
		{Label: "Videos", Value: float64(st.Videos)},
		{Label: "Storage used", Value: st.Usage, Unit: "bytes"},
	}, nil
}
