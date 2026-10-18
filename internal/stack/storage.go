package stack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A Storage is a place for the apps' files besides the data folder: a drive
// or folder on this machine, or a share on the network.
type Storage struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name,omitempty" json:"name"`
	Type string `yaml:"type" json:"type"` // StorageFolder, StorageSMB or StorageNFS

	Path   string `yaml:"path,omitempty" json:"path"`     // folder: absolute path on this machine
	Server string `yaml:"server,omitempty" json:"server"` // smb, nfs: host name or address
	Share  string `yaml:"share,omitempty" json:"share"`   // smb: share name; nfs: exported path
	User   string `yaml:"user,omitempty" json:"user"`     // smb; the password is in .env
}

const (
	StorageFolder = "folder"
	StorageSMB    = "smb"
	StorageNFS    = "nfs"
)

// Place puts an area on a storage, in a folder inside it.
type Place struct {
	Storage string `yaml:"storage" json:"storage"`         // Storage.ID, or DataFolder
	Folder  string `yaml:"folder,omitempty" json:"folder"` // relative to the storage's root
}

// DataFolder as a Place's storage means the stack's data folder. Areas
// without a place are there too, except downloads, which follow the media.
const DataFolder = "data"

// Areas group the folders below the data folder. Each area can live on its
// own storage.
//
// Downloads go with the media unless they get a place of their own. Together
// the *arr apps move finished downloads into the library instantly, with a
// hardlink or rename, which only works within one filesystem and one mount.
// Apart, for example downloads on a local disk and the library on a NAS,
// they copy them instead.
var Areas = []struct{ ID, Name string }{
	{"media", "Media"},
	{"downloads", "Downloads"},
	{"photos", "Photos"},
	{"documents", "Documents"},
	{"sync", "Synced folders"},
}

// DownloadFolders are the folders below the data folder that hold downloads.
var DownloadFolders = []string{"torrents", "usenet"}

// areaOf returns the area a path below the data folder belongs to. The
// first folder decides; anything unknown goes with the media.
func areaOf(sub string) string {
	first, _, _ := strings.Cut(sub, "/")
	for _, d := range DownloadFolders {
		if first == d {
			return "downloads"
		}
	}
	for _, a := range Areas {
		if first == a.ID && a.ID != "downloads" {
			return a.ID
		}
	}
	return "media"
}

// placeOf returns where an area lives; Storage "" is the data folder.
func (c *Config) placeOf(area string) Place {
	p, ok := c.Areas[area]
	if !ok && area == "downloads" {
		return c.placeOf("media")
	}
	if !ok || p.Storage == DataFolder {
		return Place{}
	}
	p.Folder = strings.Trim(filepath.ToSlash(p.Folder), "/")
	return p
}

// SplitDownloads reports whether downloads and media live in different
// places, so they are mounted separately and finished downloads are copied.
func (c *Config) SplitDownloads() bool {
	return c.placeOf("downloads") != c.placeOf("media")
}

var (
	storageIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	hostRe      = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	shareRe     = regexp.MustCompile(`^[^\\/:*?"<>|,=]+$`)
	userRe      = regexp.MustCompile(`^[^,=\s]+$`)
)

// StoragePasswordKey is the .env entry with the password for an SMB share.
func StoragePasswordKey(id string) string {
	return "STORAGE_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_")) + "_PASSWORD"
}

// StoragePasswordOK reports whether a password can be passed to the mount:
// the options are comma-separated, so it must not contain a comma.
func StoragePasswordOK(p string) bool { return !strings.ContainsAny(p, ",\n\r") }

func (c *Config) validateStorage() error {
	seen := map[string]bool{}
	for _, s := range c.Storage {
		if !storageIDRe.MatchString(s.ID) {
			return fmt.Errorf("storage id %q must be lowercase letters, digits or -", s.ID)
		}
		if seen[s.ID] {
			return fmt.Errorf("storage %q is defined twice", s.ID)
		}
		if s.ID == DataFolder {
			return fmt.Errorf("the storage id %q is reserved for the data folder", s.ID)
		}
		seen[s.ID] = true
		switch s.Type {
		case StorageFolder:
			if !filepath.IsAbs(s.Path) && !path.IsAbs(s.Path) {
				return fmt.Errorf("storage %s: the path must be absolute, like /mnt/usb", s.ID)
			}
		case StorageSMB, StorageNFS:
			if !hostRe.MatchString(s.Server) {
				return fmt.Errorf("storage %s: %q is not a valid server name or address", s.ID, s.Server)
			}
			if s.Type == StorageSMB {
				if !shareRe.MatchString(s.Share) {
					return fmt.Errorf("storage %s: %q is not a valid share name", s.ID, s.Share)
				}
				if s.User != "" && !userRe.MatchString(s.User) {
					return fmt.Errorf("storage %s: the user name must not contain spaces, commas or =", s.ID)
				}
			} else if !strings.HasPrefix(s.Share, "/") || strings.ContainsAny(s.Share, ",= ") {
				return fmt.Errorf("storage %s: the export must be a path like /volume1/media", s.ID)
			}
		default:
			return fmt.Errorf("storage %s: unknown type %q", s.ID, s.Type)
		}
	}
	for area, p := range c.Areas {
		known := false
		for _, a := range Areas {
			known = known || a.ID == area
		}
		if !known {
			return fmt.Errorf("unknown area %q", area)
		}
		if p.Storage != "" && p.Storage != DataFolder && !seen[p.Storage] {
			return fmt.Errorf("area %s: there is no storage %q", area, p.Storage)
		}
		if f := filepath.ToSlash(p.Folder); path.IsAbs(f) || filepath.IsAbs(p.Folder) || strings.Contains(f, "..") || strings.ContainsAny(f, ":\\") {
			return fmt.Errorf("area %s: the folder must be a name inside the storage, like media", area)
		}
	}
	return nil
}

// StorageByID returns the storage with that ID, or nil.
func (c *Config) StorageByID(id string) *Storage {
	for i := range c.Storage {
		if c.Storage[i].ID == id {
			return &c.Storage[i]
		}
	}
	return nil
}

// DataLocation says where a path below the data folder really is. Sub is
// e.g. "media/movies" or "photos"; "" is the whole media area, the mount
// that media apps get as /data.
type DataLocation struct {
	Storage *Storage // nil: the data folder
	// Root is the area's folder on that storage: below the data folder
	// (relative or absolute as configured), below a folder storage's path,
	// or inside a share (relative, "" for the share's root).
	Root string
	// Rel is the rest of the path below Root, in slash form.
	Rel string
}

// Locate resolves a path below the data folder to its storage.
func (c *Config) Locate(sub string) DataLocation {
	sub = strings.Trim(filepath.ToSlash(sub), "/")
	area := areaOf(sub)
	place := c.placeOf(area)
	if place.Storage == "" {
		return DataLocation{Root: c.DataDir, Rel: sub}
	}
	st := c.StorageByID(place.Storage)
	folder := place.Folder
	rel := sub
	if area != "media" && area != "downloads" {
		// Other areas are one folder each: "photos/x" is <folder>/x. Media
		// and downloads keep their layout (media/tv, torrents/...) inside
		// their folder, so the two can share one.
		rel = strings.TrimPrefix(strings.TrimPrefix(sub, area), "/")
	}
	if st.Type == StorageFolder {
		return DataLocation{Storage: st, Root: joinSlash(st.Path, folder), Rel: rel}
	}
	return DataLocation{Storage: st, Root: folder, Rel: rel}
}

func joinSlash(a, b string) string {
	switch {
	case b == "":
		return a
	case a == "":
		return b
	}
	return strings.TrimSuffix(a, "/") + "/" + b
}

// MountOptions returns the driver options for a network storage's Docker
// volume, without the password.
func (c *Config) MountOptions(s *Storage) map[string]string {
	switch s.Type {
	case StorageSMB:
		o := fmt.Sprintf("vers=3.0,uid=%d,gid=%d,file_mode=0664,dir_mode=0775", c.PUID, c.PGID)
		if s.User != "" {
			o = "username=" + s.User + "," + o
		} else {
			o = "guest," + o
		}
		return map[string]string{"type": "cifs", "device": "//" + s.Server + "/" + s.Share, "o": o}
	case StorageNFS:
		return map[string]string{"type": "nfs", "device": ":" + s.Share, "o": "addr=" + s.Server + ",rw,nfsvers=4"}
	}
	return nil
}

// VolumeName is the Docker volume that mounts a network storage. It
// changes with the mount options, so editing the storage creates a fresh
// volume instead of reusing one that still points to the old share.
func (c *Config) VolumeName(s *Storage) string {
	opts := c.MountOptions(s)
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, opts[k])
	}
	return fmt.Sprintf("%s-storage-%s-%s", c.Name, s.ID, hex.EncodeToString(h.Sum(nil))[:8])
}
