package wire

import (
	"errors"
	"fmt"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

// audiobookshelf creates the root account, which a fresh Audiobookshelf
// otherwise hands to whoever opens it first, and one library each for
// audiobooks and podcasts.
type audiobookshelf struct{}

func (audiobookshelf) Prepare(*Ctx, catalog.App) error { return nil }

var absLibraries = []struct {
	name, path, mediaType, provider string
}{
	{"Audiobooks", "/audiobooks", "book", "google"},
	{"Podcasts", "/podcasts", "podcast", "itunes"},
}

type absLibrary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Folders []struct {
		FullPath string `json:"fullPath"`
	} `json:"folders"`
}

func (audiobookshelf) Wire(c *Ctx, a catalog.App) error {
	base := c.BaseURL(a)
	var status struct {
		IsInit bool `json:"isInit"`
	}
	if err := doJSON(c.HTTP, "GET", base+"/status", nil, nil, &status); err != nil {
		return err
	}
	user, pass := c.Secrets.Get(stack.AdminUser), c.Secrets.Get(stack.AdminPassword)
	if status.IsInit {
		c.logf(a, "root account already exists")
	} else {
		root := map[string]any{"newRoot": map[string]string{"username": user, "password": pass}}
		if err := doJSON(c.HTTP, "POST", base+"/init", nil, root, nil); err != nil {
			return err
		}
		c.logf(a, "root account %s created", user)
	}
	auth, err := absLogin(c, base)
	if err != nil {
		return err
	}
	var libs struct {
		Libraries []absLibrary `json:"libraries"`
	}
	if err := doJSON(c.HTTP, "GET", base+"/api/libraries", auth, nil, &libs); err != nil {
		return err
	}
	for _, want := range absLibraries {
		exists := false
		for _, l := range libs.Libraries {
			for _, f := range l.Folders {
				exists = exists || f.FullPath == want.path
			}
		}
		if exists {
			c.logf(a, "library %s exists", want.name)
			continue
		}
		body := map[string]any{
			"name":      want.name,
			"folders":   []map[string]string{{"fullPath": want.path}},
			"mediaType": want.mediaType,
			"provider":  want.provider,
			"icon":      "database",
		}
		if err := doJSON(c.HTTP, "POST", base+"/api/libraries", auth, body, nil); err != nil {
			return err
		}
		c.logf(a, "library %s added (%s)", want.name, want.path)
	}
	return nil
}

// absLogin signs in with the stack's admin account and returns the header
// for API calls.
func absLogin(c *Ctx, base string) (map[string]string, error) {
	var login struct {
		User struct {
			Token       string `json:"token"`
			AccessToken string `json:"accessToken"`
		} `json:"user"`
	}
	body := map[string]string{"username": c.Secrets.Get(stack.AdminUser), "password": c.Secrets.Get(stack.AdminPassword)}
	if err := doJSON(c.HTTP, "POST", base+"/login", nil, body, &login); err != nil {
		return nil, fmt.Errorf("login with the stack's admin account failed: %w", err)
	}
	token := login.User.AccessToken
	if token == "" {
		token = login.User.Token
	}
	if token == "" {
		return nil, errors.New("login returned no token")
	}
	return map[string]string{"Authorization": "Bearer " + token}, nil
}

func absFacts(c *Ctx, a catalog.App) ([]Fact, error) {
	base := c.BaseURL(a)
	auth, err := absLogin(c, base)
	if err != nil {
		return nil, err
	}
	var libs struct {
		Libraries []absLibrary `json:"libraries"`
	}
	if err := doJSON(c.HTTP, "GET", base+"/api/libraries", auth, nil, &libs); err != nil {
		return nil, err
	}
	var facts []Fact
	for _, l := range libs.Libraries {
		var items struct {
			Total int `json:"total"`
		}
		if err := doJSON(c.HTTP, "GET", base+"/api/libraries/"+l.ID+"/items?limit=1", auth, nil, &items); err != nil {
			return nil, err
		}
		facts = append(facts, Fact{Label: l.Name, Value: float64(items.Total)})
	}
	return facts, nil
}
