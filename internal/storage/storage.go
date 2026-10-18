// Package storage connects the stack to drives and network shares: it
// creates the Docker volumes for shares, checks that the apps can write to
// a storage, and finds the drives of this machine.
package storage

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"stackwire/internal/catalog"
	"stackwire/internal/dockerx"
	"stackwire/internal/render"
	"stackwire/internal/stack"
)

// Small image used to create folders and test writing inside a mount.
const helperImage = "busybox:1.37"

const (
	labelProject = "stackwire.project"
	labelStorage = "stackwire.storage"
)

// Prepare creates the Docker volumes for the network storages the apps use
// and the folders the apps need inside them. Volumes of storages that were
// changed or removed are cleaned up. Run it before the apps start.
func Prepare(cfg *stack.Config, sec *stack.Secrets, apps []catalog.App, out io.Writer) error {
	dirs := render.NetworkDirs(cfg, apps)
	for i := range cfg.Storage {
		s := &cfg.Storage[i]
		name := cfg.VolumeName(s)
		subs, used := dirs[name]
		if !used {
			continue
		}
		if err := ensureVolume(cfg, sec, s, name); err != nil {
			return fmt.Errorf("%s: %w", displayName(s), err)
		}
		if len(subs) > 0 {
			args := []string{"run", "--rm", "-v", name + ":/v", helperImage, "sh", "-c",
				fmt.Sprintf(`mkdir -p "$@" && chown %d:%d "$@" 2>/dev/null; true`, cfg.PUID, cfg.PGID), "sh"}
			for _, d := range subs {
				args = append(args, path.Join("/v", d))
			}
			if _, err := dockerx.Output(args...); err != nil {
				return fmt.Errorf("%s: %s", displayName(s), mountError(err))
			}
		}
		fmt.Fprintf(out, "  storage      %s connected\n", displayName(s))
	}
	removeStale(cfg, dirs)
	return nil
}

// ensureVolume creates the volume, or creates it again if its options no
// longer match, for example after the share's password was changed.
func ensureVolume(cfg *stack.Config, sec *stack.Secrets, s *stack.Storage, name string) error {
	want := volumeOptions(cfg, sec, s)
	if raw, err := dockerx.Output("volume", "inspect", name, "--format", "{{json .Options}}"); err == nil {
		var have map[string]string
		if json.Unmarshal(raw, &have) == nil && equalOptions(have, want) {
			return nil
		}
		// Containers that still use it are recreated by the next start.
		if ids, err := dockerx.Output("ps", "-aq", "--filter", "volume="+name); err == nil {
			for _, id := range strings.Fields(string(ids)) {
				dockerx.Output("rm", "-f", id)
			}
		}
		if _, err := dockerx.Output("volume", "rm", name); err != nil {
			return err
		}
	}
	return createVolume(cfg, s, name, want)
}

func createVolume(cfg *stack.Config, s *stack.Storage, name string, opts map[string]string) error {
	args := []string{"volume", "create", "--driver", "local",
		"--label", labelProject + "=" + cfg.Name, "--label", labelStorage + "=" + s.ID}
	for _, k := range []string{"type", "device", "o"} {
		args = append(args, "--opt", k+"="+opts[k])
	}
	_, err := dockerx.Output(append(args, name)...)
	return err
}

func volumeOptions(cfg *stack.Config, sec *stack.Secrets, s *stack.Storage) map[string]string {
	opts := cfg.MountOptions(s)
	if s.Type == stack.StorageSMB && s.User != "" {
		opts["o"] += ",password=" + sec.Get(stack.StoragePasswordKey(s.ID))
	}
	return opts
}

func equalOptions(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

// removeStale deletes this stack's storage volumes that nothing refers to
// any more. One still in use stays; the next start tries again.
func removeStale(cfg *stack.Config, current map[string][]string) {
	out, err := dockerx.Output("volume", "ls", "-q", "--filter", "label="+labelProject+"="+cfg.Name, "--filter", "label="+labelStorage)
	if err != nil {
		return
	}
	for _, name := range strings.Fields(string(out)) {
		if _, ok := current[name]; !ok {
			dockerx.Output("volume", "rm", name)
		}
	}
}

// CheckResult is what a storage check found.
type CheckResult struct {
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// Check mounts the storage the way the apps will and writes a test file as
// the apps' user. That catches a wrong password, a missing share, and a
// drive the apps may not write to. password is used for a share instead of
// the stored one if not empty.
func Check(cfg *stack.Config, sec *stack.Secrets, s *stack.Storage, password string) (CheckResult, error) {
	var mount string
	switch s.Type {
	case stack.StorageFolder:
		mount = s.Path
	default:
		if password != "" {
			sec.Set(stack.StoragePasswordKey(s.ID), password) // only in memory
		}
		name := cfg.Name + "-storage-check-" + strings.ToLower(rand.Text()[:8])
		if err := createVolume(cfg, s, name, volumeOptions(cfg, sec, s)); err != nil {
			return CheckResult{}, err
		}
		defer dockerx.Output("volume", "rm", "-f", name)
		mount = name
	}
	dir := "/v/.stackwire-check-" + strings.ToLower(rand.Text()[:6])
	user := strconv.Itoa(cfg.PUID) + ":" + strconv.Itoa(cfg.PGID)
	// As root: a folder the apps' user owns, as stackwire creates them.
	if _, err := dockerx.Output("run", "--rm", "-v", mount+":/v", helperImage, "sh", "-c",
		fmt.Sprintf("mkdir %s && (chown %s %s 2>/dev/null; true)", dir, user, dir)); err != nil {
		return CheckResult{}, fmt.Errorf("%s", mountError(err))
	}
	defer dockerx.Output("run", "--rm", "-v", mount+":/v", helperImage, "rm", "-rf", dir)
	// As the apps' user: write into it.
	out, err := dockerx.Output("run", "--rm", "--user", user, "-v", mount+":/v", helperImage, "sh", "-c",
		fmt.Sprintf("touch %s/test && rm %s/test && df -Pk /v | tail -1", dir, dir))
	if err != nil {
		return CheckResult{}, fmt.Errorf("the apps cannot write there (they run as user %s): %s", user, mountError(err))
	}
	// df -P: filesystem, 1024-blocks, used, available, capacity, mounted on
	f := strings.Fields(string(out))
	var res CheckResult
	if len(f) >= 4 {
		total, _ := strconv.ParseUint(f[1], 10, 64)
		free, _ := strconv.ParseUint(f[3], 10, 64)
		res = CheckResult{Total: total * 1024, Free: free * 1024}
	}
	return res, nil
}

// mountError turns Docker's mount errors into what they usually mean.
// Docker reports them as "...failed to mount local volume: mount <device>:
// <path>, data: <options>: <reason>", with the password masked.
func mountError(err error) string {
	msg := err.Error()
	low := strings.ToLower(msg)
	if i := strings.Index(low, "failed to mount local volume"); i >= 0 {
		reason := strings.TrimSpace(low[strings.LastIndex(low, ": ")+2:])
		reason, _, _ = strings.Cut(reason, "\n")
		switch {
		case strings.Contains(reason, "permission denied"):
			return "the server refused the login; check user and password"
		case strings.Contains(reason, "no such file or directory"):
			return "the share or export was not found on the server"
		case strings.Contains(reason, "access denied by server"):
			return "the NFS server does not allow this machine; add its address to the export"
		case strings.Contains(reason, "in progress"), strings.Contains(reason, "host is down"), strings.Contains(reason, "no route to host"),
			strings.Contains(reason, "connection refused"), strings.Contains(reason, "timed out"), strings.Contains(reason, "network is unreachable"):
			return "the server did not answer; check its address and that file sharing is on"
		}
		return "mounting failed: " + reason
	}
	switch {
	case strings.Contains(low, "permission denied"), strings.Contains(low, "read-only file system"):
		return "permission denied"
	}
	return msg
}

func displayName(s *stack.Storage) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}
