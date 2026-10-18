//go:build !linux && !windows

package storage

import (
	"os"
	"path/filepath"
)

// drives lists what is mounted below /Volumes, as on macOS.
func drives() ([]Drive, error) {
	entries, err := os.ReadDir("/Volumes")
	if err != nil {
		return nil, nil
	}
	var list []Drive
	for _, e := range entries {
		p := filepath.Join("/Volumes", e.Name())
		total, free, err := Space(p)
		if err != nil {
			continue
		}
		list = append(list, Drive{Name: e.Name(), Path: p, Size: total, Free: free, Removable: true})
	}
	return list, nil
}
