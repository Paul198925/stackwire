package wire

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

// qbittorrent seeds the config file before the first start. Without it
// qBittorrent invents a temporary password that is only printed to its log.
type qbittorrent struct{}

func (qbittorrent) Prepare(c *Ctx, a catalog.App) error {
	// The linuxserver image keeps the file at /config/qBittorrent/qBittorrent.conf.
	path := filepath.Join(c.Cfg.Abs(c.Cfg.ConfigDir), a.ID, "qBittorrent", "qBittorrent.conf")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash, err := qbPasswordHash(c.Secrets.Get(stack.AdminPassword), salt)
	if err != nil {
		return err
	}
	conf := fmt.Sprintf(`[BitTorrent]
Session\DefaultSavePath=/data/torrents
Session\Port=6881
Session\TempPathEnabled=false

[LegalNotice]
Accepted=true

[Preferences]
WebUI\Address=*
WebUI\Port=%d
WebUI\ServerDomains=*
WebUI\Username=%s
WebUI\Password_PBKDF2="%s"
`, a.Port, c.Secrets.Get(stack.AdminUser), hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(conf), 0o644)
}

// qbPasswordHash produces the value of WebUI\Password_PBKDF2:
// PBKDF2-HMAC-SHA512 with 100000 iterations and a 64 byte key.
func qbPasswordHash(password string, salt []byte) (string, error) {
	key, err := pbkdf2.Key(sha512.New, password, salt, 100000, 64)
	if err != nil {
		return "", err
	}
	enc := base64.StdEncoding
	return "@ByteArray(" + enc.EncodeToString(salt) + ":" + enc.EncodeToString(key) + ")", nil
}

// Wire only verifies the login. If it fails, the *arr apps could not connect
// either, and the message here is clearer than theirs.
func (qbittorrent) Wire(c *Ctx, a catalog.App) error {
	base := c.BaseURL(a)
	form := url.Values{
		"username": {c.Secrets.Get(stack.AdminUser)},
		"password": {c.Secrets.Get(stack.AdminPassword)},
	}
	req, err := http.NewRequest("POST", base+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", base)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 300 || strings.HasPrefix(strings.TrimSpace(string(body)), "Fails") {
		return fmt.Errorf("login with the stack's admin account failed (%s); "+
			"if this qBittorrent was set up by hand, set its WebUI login to the values in %s",
			resp.Status, stack.SecretsFile)
	}
	c.logf(a, "login verified")
	return nil
}
