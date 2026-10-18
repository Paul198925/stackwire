package storage

// A Drive is a filesystem of this machine that can hold the apps' files.
type Drive struct {
	Name      string `json:"name"`      // label, model or device
	Device    string `json:"device"`    // e.g. /dev/sdb1, or C:
	Path      string `json:"path"`      // where it is mounted; "" if not mounted
	FS        string `json:"fs"`        // filesystem type, if known
	Size      uint64 `json:"size"`      // bytes
	Free      uint64 `json:"free"`      // bytes, if mounted
	Removable bool   `json:"removable"` // USB and other removable drives
}

// Drives lists the drives of this machine, leaving out the ones the system
// itself runs from, such as the boot partition.
func Drives() ([]Drive, error) { return drives() }
