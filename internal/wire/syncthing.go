package wire

import (
	"errors"
	"regexp"
	"time"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

const syncthingKeySecret = "SYNCTHING_API_KEY"

var syncthingKeyRe = regexp.MustCompile(`<apikey>([^<]+)</apikey>`)

// syncthing protects the web interface with the stack's login and makes new
// shared folders land in the data folder instead of the config folder.
type syncthing struct{}

func (syncthing) Prepare(*Ctx, catalog.App) error { return nil }

func (syncthing) Wire(c *Ctx, a catalog.App) error {
	key, err := keyFromConfig(c, a, "config.xml", syncthingKeySecret, syncthingKeyRe)
	if err != nil {
		return err
	}
	base := c.BaseURL(a) + "/rest"
	header := map[string]string{"X-API-Key": key}

	var gui struct {
		User string `json:"user"`
	}
	if err := doJSON(c.HTTP, "GET", base+"/config/gui", header, nil, &gui); err != nil {
		return err
	}
	if gui.User != "" {
		c.logf(a, "login already configured")
	} else {
		// Syncthing hashes the password itself.
		login := map[string]string{"user": c.Secrets.Get(stack.AdminUser), "password": c.Secrets.Get(stack.AdminPassword)}
		if err := doJSON(c.HTTP, "PATCH", base+"/config/gui", header, login, nil); err != nil {
			return err
		}
		c.logf(a, "login enabled for user %s", login["user"])
		// Syncthing restarts its web interface to apply this.
		deadline := time.Now().Add(30 * time.Second)
		time.Sleep(time.Second)
		for !Healthy(c, a) {
			if time.Now().After(deadline) {
				return errors.New("web interface did not come back after enabling the login")
			}
			time.Sleep(time.Second)
		}
	}

	var defaults struct {
		Path string `json:"path"`
	}
	if err := doJSON(c.HTTP, "GET", base+"/config/defaults/folder", header, nil, &defaults); err != nil {
		return err
	}
	if defaults.Path == "/sync" {
		return nil
	}
	if err := doJSON(c.HTTP, "PATCH", base+"/config/defaults/folder", header, map[string]string{"path": "/sync"}, nil); err != nil {
		return err
	}
	c.logf(a, "new folders are created in the data folder (sync)")
	return nil
}

func syncthingFacts(c *Ctx, a catalog.App) ([]Fact, error) {
	key := c.Secrets.Get(syncthingKeySecret)
	if key == "" {
		return nil, nil
	}
	base := c.BaseURL(a) + "/rest"
	header := map[string]string{"X-API-Key": key}
	var folders, devices []map[string]any
	if err := doJSON(c.HTTP, "GET", base+"/config/folders", header, nil, &folders); err != nil {
		return nil, err
	}
	if err := doJSON(c.HTTP, "GET", base+"/config/devices", header, nil, &devices); err != nil {
		return nil, err
	}
	var conns struct {
		Connections map[string]struct {
			Connected bool `json:"connected"`
		} `json:"connections"`
	}
	if err := doJSON(c.HTTP, "GET", base+"/system/connections", header, nil, &conns); err != nil {
		return nil, err
	}
	connected := 0
	for _, x := range conns.Connections {
		if x.Connected {
			connected++
		}
	}
	others := len(devices) - 1 // the list includes this device
	if others < 0 {
		others = 0
	}
	return []Fact{
		{Label: "Folders", Value: float64(len(folders))},
		{Label: "Devices", Value: float64(others)},
		{Label: "Connected now", Value: float64(connected)},
	}, nil
}
