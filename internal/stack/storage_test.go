package stack

import "testing"

func storageConfig(t *testing.T) *Config {
	t.Helper()
	c := Default(t.TempDir())
	c.Storage = []Storage{
		{ID: "usb", Type: StorageFolder, Path: "/mnt/usb"},
		{ID: "nas", Type: StorageSMB, Server: "nas.local", Share: "media", User: "paul"},
	}
	c.Areas = map[string]Place{"media": {Storage: "nas", Folder: "stackwire"}, "photos": {Storage: "usb", Folder: "pics"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLocate(t *testing.T) {
	c := storageConfig(t)
	for _, tc := range []struct{ sub, storage, root, rel string }{
		{"", "nas", "stackwire", ""},
		{"media/movies", "nas", "stackwire", "media/movies"},
		{"torrents", "nas", "stackwire", "torrents"}, // downloads go with the media
		{"photos", "usb", "/mnt/usb/pics", ""},
		{"photos/library", "usb", "/mnt/usb/pics", "library"},
		{"documents/inbox", "", "./data", "documents/inbox"}, // no place: the data folder
	} {
		loc := c.Locate(tc.sub)
		id := ""
		if loc.Storage != nil {
			id = loc.Storage.ID
		}
		if id != tc.storage || loc.Root != tc.root || loc.Rel != tc.rel {
			t.Errorf("Locate(%q) = %s %q %q, want %s %q %q", tc.sub, id, loc.Root, loc.Rel, tc.storage, tc.root, tc.rel)
		}
	}
}

func TestStorageValidation(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"relative path":      func(c *Config) { c.Storage[0].Path = "usb" },
		"bad id":             func(c *Config) { c.Storage[0].ID = "My USB" },
		"duplicate id":       func(c *Config) { c.Storage[1].ID = "usb" },
		"comma in user":      func(c *Config) { c.Storage[1].User = "a,b" },
		"nfs without slash":  func(c *Config) { c.Storage[1].Type, c.Storage[1].Share = StorageNFS, "volume1" },
		"unknown storage":    func(c *Config) { c.Areas["sync"] = Place{Storage: "cloud"} },
		"unknown area":       func(c *Config) { c.Areas["games"] = Place{Storage: "usb"} },
		"folder leaves root": func(c *Config) { c.Areas["photos"] = Place{Storage: "usb", Folder: "../etc"} },
	} {
		c := storageConfig(t)
		change(c)
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVolumeNameFollowsOptions(t *testing.T) {
	c := storageConfig(t)
	a := c.VolumeName(&c.Storage[1])
	c.Storage[1].Server = "192.168.1.5"
	if b := c.VolumeName(&c.Storage[1]); a == b {
		t.Error("a changed server must give a new volume")
	}
}
