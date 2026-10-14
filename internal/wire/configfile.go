package wire

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"stackwire/internal/catalog"
)

// keyFromConfig reads an API key that an app generated on its first start
// from its config file, which lives in the stack folder, and keeps it in the
// secrets so other apps and the dashboard can use it. re must have one group.
func keyFromConfig(c *Ctx, a catalog.App, file, secret string, re *regexp.Regexp) (string, error) {
	path := filepath.Join(c.Cfg.Abs(c.Cfg.ConfigDir), a.ID, file)
	var key string
	// The file can lag behind the health check by a moment.
	for i := 0; i < 15 && key == ""; i++ {
		if i > 0 {
			time.Sleep(time.Second)
		}
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if m := re.FindSubmatch(b); m != nil {
			key = string(m[1])
		}
	}
	if key == "" {
		return "", fmt.Errorf("no API key found in %s", path)
	}
	if c.Secrets.Get(secret) != key {
		c.Secrets.Set(secret, key)
		if err := c.Secrets.Save(); err != nil {
			return "", err
		}
	}
	return key, nil
}
