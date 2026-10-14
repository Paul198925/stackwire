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
	if fs, ok := c.app("flaresolverr"); ok {
		return cl.ensureFlareSolverr(c, a, fs)
	}
	return nil
}

const flareSolverrTag = "flaresolverr"

// ensureFlareSolverr registers FlareSolverr as an indexer proxy. Prowlarr
// applies a proxy to the indexers that share a tag with it, so the user
// only has to give an indexer the tag.
func (cl *arrClient) ensureFlareSolverr(c *Ctx, a, fs catalog.App) error {
	res, exists, err := cl.fromSchema("/indexerproxy", "FlareSolverr")
	if err != nil {
		return err
	}
	if exists {
		c.logf(a, "FlareSolverr proxy exists")
		return nil
	}
	var tags []map[string]any
	if err := cl.do("GET", "/tag", nil, &tags); err != nil {
		return err
	}
	var tagID any
	for _, t := range tags {
		if t["label"] == flareSolverrTag {
			tagID = t["id"]
		}
	}
	if tagID == nil {
		var created map[string]any
		if err := cl.do("POST", "/tag", map[string]any{"label": flareSolverrTag}, &created); err != nil {
			return err
		}
		tagID = created["id"]
	}
	res["name"] = "FlareSolverr"
	res["tags"] = []any{tagID}
	if !setField(res, "host", fmt.Sprintf("http://%s:%d/", fs.ID, fs.Port)) {
		return errors.New("indexer proxy schema has no field \"host\"")
	}
	if err := cl.do("POST", "/indexerproxy", res, nil); err != nil {
		return err
	}
	c.logf(a, "FlareSolverr proxy added; give an indexer the tag %q to use it", flareSolverrTag)
	return nil
}
