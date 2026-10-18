package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"stackwire/internal/catalog"
	"stackwire/internal/ops"
	"stackwire/internal/stack"
)

// SetupFile holds the one-time code for the first-visit setup. It exists
// from the moment `serve` creates a new stack until the setup is finished.
// Whoever opens the page first must know the code, so nobody else on the
// network can claim a freshly installed server.
const SetupFile = ".setup-code"

// The chosen password ends up in .env and in the apps' config files, so
// only characters that need no escaping in any of them are allowed.
var passwordRe = regexp.MustCompile(`^[A-Za-z0-9._~!@+=,:^*-]{12,64}$`)

// StartSetup creates the setup code for a new stack and returns it.
func StartSetup(dir string) (string, error) {
	code := rand.Text()
	return code, os.WriteFile(filepath.Join(dir, SetupFile), []byte(code+"\n"), 0o600)
}

// SetupCode returns the pending setup code, or "" once the setup is done.
func SetupCode(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, SetupFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type setupApp struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Internal    bool     `json:"internal"`
	Requires    []string `json:"requires"` // IDs
}

// handleSetupInfo tells the page whether to show the setup. With the right
// code it also returns what the setup form needs.
func (s *Server) handleSetupInfo(w http.ResponseWriter, r *http.Request) {
	code := SetupCode(s.dir)
	if code == "" {
		writeJSON(w, map[string]bool{"pending": false})
		return
	}
	given := r.URL.Query().Get("code")
	if given == "" {
		writeJSON(w, map[string]bool{"pending": true})
		return
	}
	if !s.checkSetupCode(w, given, code) {
		return
	}
	st, err := ops.Load(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	all, err := catalog.All()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	apps := make([]setupApp, 0, len(all))
	for _, a := range all {
		apps = append(apps, setupApp{a.ID, a.Name, a.Description, a.Category, a.Internal, a.Requires})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{
		"pending": true, "codeOK": true, "apps": apps,
		"adminUser": st.Secrets.Get(stack.AdminUser),
		// Offered as a start; the person can replace it.
		"password": st.Secrets.Get(stack.AdminPassword),
		"timezone": st.Cfg.Timezone, "dataDir": st.Cfg.DataDir, "dir": st.Cfg.Dir,
	})
}

// checkSetupCode compares the code and counts wrong ones like failed logins.
func (s *Server) checkSetupCode(w http.ResponseWriter, given, code string) bool {
	if s.tooManyFailures() {
		fail(w, http.StatusTooManyRequests, "too many failed attempts, try again in a minute")
		return false
	}
	if subtle.ConstantTimeCompare([]byte(given), []byte(code)) != 1 {
		s.mu.Lock()
		s.failed = append(s.failed, time.Now())
		s.mu.Unlock()
		time.Sleep(time.Second)
		fail(w, http.StatusForbidden, "this setup code is not valid; use the link the installer printed")
		return false
	}
	return true
}

// handleSetup saves the choices from the setup, signs the person in and
// starts installing the apps.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code, Password, Domain, Email, Timezone, DataDir string
		Apps                                             []string
	}
	if r.Header.Get(csrfHeader) == "" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	code := SetupCode(s.dir)
	if code == "" {
		fail(w, http.StatusConflict, "the setup is already done; sign in instead")
		return
	}
	if !s.checkSetupCode(w, in.Code, code) {
		return
	}
	if s.jobRunning() {
		fail(w, http.StatusConflict, "wait until the running task has finished")
		return
	}
	if err := s.applySetup(in.Password, in.Domain, in.Email, in.Timezone, in.DataDir, in.Apps); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.Remove(filepath.Join(s.dir, SetupFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.startSession(w, r)
	s.startJob(w, "Installing your apps", func(out io.Writer) error {
		st, err := ops.Load(s.dir)
		if err != nil {
			return err
		}
		return s.Apply(st, out)
	})
}

func (s *Server) applySetup(password, domain, email, timezone, dataDir string, apps []string) error {
	if !passwordRe.MatchString(password) {
		return errors.New("the password needs 12 to 64 characters: letters, digits and . _ ~ ! @ + = , : ^ * -")
	}
	cfg, err := stack.Load(s.dir)
	if err != nil {
		return err
	}
	// The password becomes every app's login, so it can only change while
	// no app has been set up with the old one.
	if containers, err := s.Containers(cfg.Name); err != nil {
		return err
	} else if len(containers) > 0 {
		return errors.New("apps of this stack already ran with the current password; sign in with it instead")
	}
	cfg.Domain = strings.ToLower(strings.TrimSpace(domain))
	cfg.Email = strings.TrimSpace(email)
	if cfg.Domain != "" && cfg.Email == "" {
		return errors.New("with a domain, an email address for the certificates is needed")
	}
	if tz := strings.TrimSpace(timezone); tz != "" {
		cfg.Timezone = tz
	}
	if d := strings.TrimSpace(dataDir); d != "" {
		cfg.DataDir = d
	}
	if cfg.Apps, err = catalog.WithRequirements(apps); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	// With a domain, the proxy alone already makes this page reachable.
	if len(cfg.Apps) == 0 && cfg.Domain == "" {
		return errors.New("choose at least one app")
	}
	secrets, err := stack.LoadSecrets(s.dir)
	if err != nil {
		return err
	}
	secrets.Set(stack.AdminPassword, password)
	if err := secrets.Save(); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving %s: %w", stack.ConfigFile, err)
	}
	return nil
}
