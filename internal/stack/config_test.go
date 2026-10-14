package stack

import (
	"path/filepath"
	"testing"
)

func TestProjectName(t *testing.T) {
	for dir, want := range map[string]string{
		"/opt/media":             "media",
		"/srv/My Stack":          "mystack",
		"/home/x/home-server":    "home-server",
		"/data/__stack":          "stack",
		"/data/Ümlaut":           "mlaut",
		"/data/---":              "stackwire",
		filepath.Join("C:", "x"): "x",
	} {
		if got := projectName(dir); got != want {
			t.Errorf("projectName(%q) = %q, want %q", dir, got, want)
		}
		if c := Default(dir); c.Validate() != nil {
			t.Errorf("default config for %q is invalid: %v", dir, c.Validate())
		}
	}
}
