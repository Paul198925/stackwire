package storage

import (
	"encoding/json"
	"os/exec"
	"strings"
)

type lsblkDev struct {
	Name       string     `json:"name"`
	Path       string     `json:"path"`
	Size       uint64     `json:"size"`
	FSType     string     `json:"fstype"`
	Mountpoint string     `json:"mountpoint"`
	Label      string     `json:"label"`
	Model      string     `json:"model"`
	Tran       string     `json:"tran"`
	RM         any        `json:"rm"` // bool or "0"/"1", depending on the lsblk version
	Type       string     `json:"type"`
	Children   []lsblkDev `json:"children"`
}

func drives() ([]Drive, error) {
	out, err := exec.Command("lsblk", "-J", "-b", "-o", "NAME,PATH,SIZE,FSTYPE,MOUNTPOINT,LABEL,MODEL,TRAN,RM,TYPE").Output()
	if err != nil {
		return nil, err
	}
	var res struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	var list []Drive
	var walk func(d lsblkDev, parent lsblkDev)
	walk = func(d lsblkDev, parent lsblkDev) {
		for _, c := range d.Children {
			walk(c, d)
		}
		if d.FSType == "" || skipFS[d.FSType] || d.Type == "loop" || systemMount(d.Mountpoint) {
			return
		}
		name := d.Label
		if name == "" {
			name = strings.TrimSpace(parent.Model)
		}
		if name == "" {
			name = strings.TrimSpace(d.Model)
		}
		if name == "" {
			name = d.Name
		}
		dr := Drive{Name: name, Device: d.Path, Path: d.Mountpoint, FS: d.FSType, Size: d.Size,
			Removable: isTrue(d.RM) || isTrue(parent.RM) || d.Tran == "usb" || parent.Tran == "usb"}
		if dr.Path != "" {
			if _, free, err := Space(dr.Path); err == nil {
				dr.Free = free
			}
		}
		list = append(list, dr)
	}
	for _, d := range res.Blockdevices {
		walk(d, lsblkDev{})
	}
	return list, nil
}

var skipFS = map[string]bool{"swap": true, "LVM2_member": true, "linux_raid_member": true, "crypto_LUKS": true, "squashfs": true, "iso9660": true}

// systemMount reports whether the system itself runs from that mount.
func systemMount(p string) bool {
	switch {
	case p == "/", p == "/boot", strings.HasPrefix(p, "/boot/"), strings.HasPrefix(p, "/snap/"),
		strings.HasPrefix(p, "/var/lib/"), p == "[SWAP]", p == "/usr", p == "/var":
		return true
	}
	return false
}

func isTrue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "1" || x == "true"
	case float64:
		return x == 1
	}
	return false
}
