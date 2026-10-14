package wire

import (
	"errors"
	"fmt"

	"stackwire/internal/catalog"
)

// prowlarr registers Sonarr and Radarr as applications, so every indexer
// added in Prowlarr is synced to them.
type prowlarr struct{}

func (prowlarr) Prepare(*Ctx, catalog.App) error { return nil }

func (prowlarr) Wire(c *Ctx, a catalog.App) error {
	if a.Arr == nil {
		return errors.New("manifest has no arr section")
	}
	cl := newArrClient(c, a)
	if err := cl.ensureAuth(c, a); err != nil {
		return err
	}
	for _, target := range c.Apps {
		if target.Arr == nil || target.Arr.ProwlarrApp == "" {
			continue
		}
		res, exists, err := cl.fromSchema("/applications", target.Arr.ProwlarrApp)
		if err != nil {
			return err
		}
		if exists {
			c.logf(a, "application %s exists", target.Name)
			continue
		}
		res["name"] = target.Name
		res["syncLevel"] = "fullSync"
		for name, val := range map[string]any{
			"prowlarrUrl": fmt.Sprintf("http://%s:%d", a.ID, a.Port),
			"baseUrl":     fmt.Sprintf("http://%s:%d", target.ID, target.Port),
			"apiKey":      c.Secrets.Get(target.Arr.APIKeySecret),
		} {
			if !setField(res, name, val) {
				return fmt.Errorf("application schema has no field %q", name)
			}
		}
		if err := cl.do("POST", "/applications", res, nil); err != nil {
			return err
		}
		c.logf(a, "application %s added", target.Name)
	}
	return nil
}
