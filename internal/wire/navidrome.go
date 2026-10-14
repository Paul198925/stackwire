package wire

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

// navidromeUser is the account Navidrome creates on its first start from
// ND_DEVAUTOCREATEADMINPASSWORD. The name is fixed by Navidrome.
const navidromeUser = "admin"

// navidrome needs no setup beyond its manifest. Wire checks the login.
type navidrome struct{}

func (navidrome) Prepare(*Ctx, catalog.App) error { return nil }

func (navidrome) Wire(c *Ctx, a catalog.App) error {
	if _, err := navidromeFacts(c, a); err != nil {
		return fmt.Errorf("login as %s failed: %w", navidromeUser, err)
	}
	c.logf(a, "login verified (user %s)", navidromeUser)
	return nil
}

// navidromeFacts uses the Subsonic API, which signs each request with a
// salted hash of the password.
func navidromeFacts(c *Ctx, a catalog.App) ([]Fact, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	s := hex.EncodeToString(salt)
	sum := md5.Sum([]byte(c.Secrets.Get(stack.AdminPassword) + s))
	q := url.Values{
		"u": {navidromeUser}, "t": {hex.EncodeToString(sum[:])}, "s": {s},
		"v": {"1.16.1"}, "c": {"stackwire"}, "f": {"json"},
	}
	var resp struct {
		R struct {
			Status string `json:"status"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
			ScanStatus struct {
				Count int `json:"count"`
			} `json:"scanStatus"`
		} `json:"subsonic-response"`
	}
	if err := doJSON(c.HTTP, "GET", c.BaseURL(a)+"/rest/getScanStatus?"+q.Encode(), nil, nil, &resp); err != nil {
		return nil, err
	}
	if resp.R.Status != "ok" {
		return nil, fmt.Errorf("navidrome: %s", resp.R.Error.Message)
	}
	return []Fact{{Label: "Songs", Value: float64(resp.R.ScanStatus.Count)}}, nil
}
