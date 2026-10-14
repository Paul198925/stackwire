package wire

import (
	"fmt"
	"net/http"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

// paperless needs no setup: its admin account comes from environment
// variables in the manifest. Wire only checks that the login works.
type paperless struct{}

func (paperless) Prepare(*Ctx, catalog.App) error { return nil }

func (paperless) Wire(c *Ctx, a catalog.App) error {
	if _, err := paperlessFacts(c, a); err != nil {
		return fmt.Errorf("login with the stack's admin account failed: %w", err)
	}
	c.logf(a, "login verified")
	return nil
}

func paperlessFacts(c *Ctx, a catalog.App) ([]Fact, error) {
	req, err := http.NewRequest("GET", c.BaseURL(a)+"/api/statistics/", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.Secrets.Get(stack.AdminUser), c.Secrets.Get(stack.AdminPassword))
	req.Header.Set("Accept", "application/json")
	var st struct {
		Total int `json:"documents_total"`
		Inbox int `json:"documents_inbox"`
	}
	if err := doRequest(c.HTTP, req, &st); err != nil {
		return nil, err
	}
	return []Fact{
		{Label: "Documents", Value: float64(st.Total)},
		{Label: "In inbox", Value: float64(st.Inbox)},
	}, nil
}
