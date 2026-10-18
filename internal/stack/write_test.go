package stack

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Readers must never see an empty file while it is being saved again.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadSecrets(dir)
	if err := s.EnsureAdmin(); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	want := s.Get(AdminPassword)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s.Set("COUNTER", string(rune('a'+i%26)))
			if err := s.Save(); err != nil {
				t.Error(err)
				return
			}
		}
		close(stop)
	}()
	for {
		select {
		case <-stop:
			wg.Wait()
			return
		default:
		}
		got, err := LoadSecrets(dir)
		if err != nil {
			continue // Windows: the file can be in the middle of a rename
		}
		if got.Get(AdminPassword) != want {
			t.Fatalf("read the file half-written: password = %q", got.Get(AdminPassword))
		}
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := Default(dir)
	for i := 0; i < 3; i++ {
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != ConfigFile {
		t.Errorf("files = %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, ConfigFile)); err != nil {
		t.Error(err)
	}
}
