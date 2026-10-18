package stack

import (
	"bytes"
	"os"
	"path/filepath"
	"time"
)

// writeFile replaces a file atomically, so a concurrent reader sees either
// the old or the new content, never an empty or half-written file. Writing
// content that is already there is skipped.
func writeFile(path string, data []byte, perm os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	// On Windows the rename fails while another process has the file open;
	// readers only hold it for a moment.
	for i := 0; ; i++ {
		err = os.Rename(tmp.Name(), path)
		if err == nil || i == 20 {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
}
