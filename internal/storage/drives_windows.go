package storage

import "os"

// drives lists the drive letters. Docker Desktop can mount any of them.
func drives() ([]Drive, error) {
	var list []Drive
	for c := 'C'; c <= 'Z'; c++ {
		root := string(c) + `:\`
		if _, err := os.Stat(root); err != nil {
			continue
		}
		total, free, err := Space(root)
		if err != nil {
			continue
		}
		list = append(list, Drive{Name: "Drive " + string(c) + ":", Device: string(c) + ":", Path: root, Size: total, Free: free, Removable: c != 'C'})
	}
	return list, nil
}
