package wire

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"stackwire/internal/catalog"
	"stackwire/internal/stack"
)

// arr drives Sonarr and Radarr. Both expose the same API shape; the
// differences are in the manifest.
type arr struct{}

func (arr) Prepare(*Ctx, catalog.App) error { return nil }

func (arr) Wire(c *Ctx, a catalog.App) error {
	if a.Arr == nil {
		return errors.New("manifest has no arr section")
	}
	cl := newArrClient(c, a)
	if err := cl.ensureAuth(c, a); err != nil {
		return err
	}
	if a.Arr.RootFolder != "" {
		if err := cl.ensureRootFolder(c, a); err != nil {
			return err
		}
	}
	if qb, ok := c.app("qbittorrent"); ok {
		err := cl.ensureDownloadClient(c, a, qb, "QBittorrent", map[string]any{
			"username": c.Secrets.Get(stack.AdminUser),
			"password": c.Secrets.Get(stack.AdminPassword),
		})
		if err != nil {
			return err
		}
	}
	if sab, ok := c.app("sabnzbd"); ok {
		// Runs after the sabnzbd driver, which stores the key.
		err := cl.ensureDownloadClient(c, a, sab, "Sabnzbd", map[string]any{"apiKey": c.Secrets.Get(sabnzbdKeySecret)})
		if err != nil {
			return err
		}
	}
	if jf, ok := c.app("jellyfin"); ok && c.jellyfinKey != "" {
		if err := cl.ensureJellyfinNotification(c, a, jf); err != nil {
			return err
		}
	}
	return nil
}

type arrClient struct {
	base string
	key  string
	hc   *http.Client
}

func newArrClient(c *Ctx, a catalog.App) *arrClient {
	return &arrClient{
		base: c.BaseURL(a) + "/api/" + a.Arr.APIVersion,
		key:  c.Secrets.Get(a.Arr.APIKeySecret),
		hc:   c.HTTP,
	}
}

func (cl *arrClient) do(method, path string, in, out any) error {
	return doJSON(cl.hc, method, cl.base+path, map[string]string{"X-Api-Key": cl.key}, in, out)
}

// ensureAuth turns on the login form with the stack's admin account. A fresh
// install has no login at all and asks the first visitor to create one.
func (cl *arrClient) ensureAuth(c *Ctx, a catalog.App) error {
	var host map[string]any
	if err := cl.do("GET", "/config/host", nil, &host); err != nil {
		return err
	}
	user := c.Secrets.Get(stack.AdminUser)
	method, _ := host["authenticationMethod"].(string)
	if !strings.EqualFold(method, "none") && method != "" {
		c.logf(a, "login already configured (%s)", method)
		return nil
	}
	host["authenticationMethod"] = "forms"
	host["authenticationRequired"] = "enabled"
	host["username"] = user
	host["password"] = c.Secrets.Get(stack.AdminPassword)
	host["passwordConfirmation"] = c.Secrets.Get(stack.AdminPassword)
	if err := cl.do("PUT", fmt.Sprintf("/config/host/%v", host["id"]), host, nil); err != nil {
		return err
	}
	c.logf(a, "login enabled for user %s", user)
	return nil
}

func (cl *arrClient) ensureRootFolder(c *Ctx, a catalog.App) error {
	var folders []map[string]any
	if err := cl.do("GET", "/rootfolder", nil, &folders); err != nil {
		return err
	}
	want := strings.TrimSuffix(a.Arr.RootFolder, "/")
	for _, f := range folders {
		if p, _ := f["path"].(string); strings.TrimSuffix(p, "/") == want {
			c.logf(a, "root folder %s exists", want)
			return nil
		}
	}
	body := map[string]any{"path": want}
	if a.Arr.RootFolderName != "" {
		quality, err := cl.firstID("/qualityprofile")
		if err != nil {
			return err
		}
		metadata, err := cl.firstID("/metadataprofile")
		if err != nil {
			return err
		}
		body["name"] = a.Arr.RootFolderName
		body["defaultQualityProfileId"], body["defaultMetadataProfileId"] = quality, metadata
		body["defaultMonitorOption"], body["defaultNewItemMonitorOption"] = "all", "all"
		body["defaultTags"] = []any{}
	}
	if err := cl.do("POST", "/rootfolder", body, nil); err != nil {
		return err
	}
	c.logf(a, "root folder %s added", want)
	return nil
}

// firstID returns the ID of the first resource in a list endpoint.
func (cl *arrClient) firstID(path string) (any, error) {
	var list []map[string]any
	if err := cl.do("GET", path, nil, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return list[0]["id"], nil
}

// ensureDownloadClient adds a download client of the given implementation,
// with the app's category and the credentials in auth.
func (cl *arrClient) ensureDownloadClient(c *Ctx, a, dl catalog.App, implementation string, auth map[string]any) error {
	res, exists, err := cl.fromSchema("/downloadclient", implementation)
	if err != nil {
		return err
	}
	if exists {
		c.logf(a, "download client %s exists", dl.Name)
		return nil
	}
	res["name"] = dl.Name
	res["enable"] = true
	fields := map[string]any{"host": dl.ID, "port": dl.Port}
	for k, v := range auth {
		fields[k] = v
	}
	for name, val := range fields {
		if !setField(res, name, val) {
			return fmt.Errorf("download client schema has no field %q", name)
		}
	}
	// The category field is named after the media type: tvCategory in Sonarr,
	// movieCategory in Radarr, musicCategory in Lidarr.
	if a.Arr.Category != "" {
		set := false
		for _, name := range []string{"tvCategory", "movieCategory", "musicCategory", "category"} {
			set = setField(res, name, a.Arr.Category) || set
		}
		if !set {
			return errors.New("download client schema has no category field")
		}
	}
	if err := cl.do("POST", "/downloadclient", res, nil); err != nil {
		return err
	}
	c.logf(a, "download client %s added (category %s)", dl.Name, a.Arr.Category)
	return nil
}

// ensureJellyfinNotification makes the app tell Jellyfin to rescan as soon as
// a download is imported, instead of waiting for the next scheduled scan.
func (cl *arrClient) ensureJellyfinNotification(c *Ctx, a, jf catalog.App) error {
	res, exists, err := cl.fromSchema("/notification", "MediaBrowser")
	if err != nil {
		return err
	}
	if exists {
		c.logf(a, "Jellyfin notification exists")
		return nil
	}
	res["name"] = "Jellyfin"
	for name, val := range map[string]any{"host": jf.ID, "apiKey": c.jellyfinKey} {
		if !setField(res, name, val) {
			return fmt.Errorf("notification schema has no field %q", name)
		}
	}
	setField(res, "port", jf.Port)
	setField(res, "useSsl", false)
	setField(res, "updateLibrary", true)
	for _, ev := range []string{"Download", "Upgrade", "Rename"} {
		if ok, _ := res["supportsOn"+ev].(bool); ok {
			res["on"+ev] = true
		}
	}
	if err := cl.do("POST", "/notification", res, nil); err != nil {
		return err
	}
	c.logf(a, "Jellyfin notification added")
	return nil
}

// fromSchema returns a new resource built from the app's own schema for the
// given implementation, or exists=true if one is configured already. Building
// from the schema keeps this working when an app adds or renames fields.
func (cl *arrClient) fromSchema(path, implementation string) (res map[string]any, exists bool, err error) {
	var current []map[string]any
	if err := cl.do("GET", path, nil, &current); err != nil {
		return nil, false, err
	}
	for _, r := range current {
		if r["implementation"] == implementation {
			return nil, true, nil
		}
	}
	var schema []map[string]any
	if err := cl.do("GET", path+"/schema", nil, &schema); err != nil {
		return nil, false, err
	}
	for _, r := range schema {
		if r["implementation"] == implementation {
			delete(r, "presets")
			delete(r, "id")
			return r, false, nil
		}
	}
	return nil, false, fmt.Errorf("%s: implementation %s not offered", path, implementation)
}

// setField sets a value in a Servarr resource's field list.
func setField(res map[string]any, name string, val any) bool {
	fields, _ := res["fields"].([]any)
	for _, f := range fields {
		if m, ok := f.(map[string]any); ok && m["name"] == name {
			m["value"] = val
			return true
		}
	}
	return false
}
