package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"stackwire/internal/stack"
	"stackwire/internal/storage"
)

type storageView struct {
	stack.Storage
	HasPassword bool   `json:"hasPassword"`
	Present     bool   `json:"present"` // folder: the path exists
	Total       uint64 `json:"total"`   // folder only; shares are measured by a check
	Free        uint64 `json:"free"`
}

type areaView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Storage string `json:"storage"`
	Folder  string `json:"folder"`
}

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	cfg, err := stack.Load(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	sec, err := stack.LoadSecrets(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := struct {
		Storage    []storageView   `json:"storage"`
		Areas      []areaView      `json:"areas"`
		Drives     []storage.Drive `json:"drives"`
		DataDir    string          `json:"dataDir"`
		DriveError string          `json:"driveError,omitempty"`
	}{Storage: []storageView{}, DataDir: cfg.Abs(cfg.DataDir)}
	for _, st := range cfg.Storage {
		v := storageView{Storage: st, HasPassword: sec.Get(stack.StoragePasswordKey(st.ID)) != ""}
		if st.Type == stack.StorageFolder {
			if fi, err := os.Stat(st.Path); err == nil && fi.IsDir() {
				v.Present = true
				v.Total, v.Free, _ = storage.Space(st.Path)
			}
		}
		resp.Storage = append(resp.Storage, v)
	}
	for _, a := range stack.Areas {
		p := cfg.Areas[a.ID]
		resp.Areas = append(resp.Areas, areaView{a.ID, a.Name, p.Storage, p.Folder})
	}
	if resp.Drives, err = storage.Drives(); err != nil {
		resp.DriveError = err.Error()
	}
	if resp.Drives == nil {
		resp.Drives = []storage.Drive{}
	}
	writeJSON(w, resp)
}

// handleStorageSave replaces the list of storages and where each area
// lives. Passwords are only sent when they are set or changed.
func (s *Server) handleStorageSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Storage   []stack.Storage        `json:"storage"`
		Areas     map[string]stack.Place `json:"areas"`
		Passwords map[string]string      `json:"passwords"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	if s.jobRunning() {
		fail(w, http.StatusConflict, "wait until the running task has finished")
		return
	}
	cfg, err := stack.Load(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	sec, err := stack.LoadSecrets(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range in.Storage {
		st := &in.Storage[i]
		st.ID, st.Name = strings.TrimSpace(st.ID), strings.TrimSpace(st.Name)
		st.Path, st.Server = strings.TrimSpace(st.Path), strings.TrimSpace(st.Server)
		st.Share, st.User = strings.Trim(strings.TrimSpace(st.Share), `\`), strings.TrimSpace(st.User)
		if st.Type != stack.StorageSMB {
			st.User = ""
		}
	}
	areas := map[string]stack.Place{}
	for id, p := range in.Areas {
		if p.Storage = strings.TrimSpace(p.Storage); p.Storage != "" {
			p.Folder = strings.Trim(strings.TrimSpace(p.Folder), "/")
			areas[id] = p
		}
	}
	cfg.Storage, cfg.Areas = in.Storage, areas
	if len(cfg.Areas) == 0 {
		cfg.Areas = nil
	}
	if err := cfg.Validate(); err != nil {
		fail(w, http.StatusBadRequest, friendlyStorageError(err))
		return
	}
	keep := map[string]bool{}
	for _, st := range cfg.Storage {
		key := stack.StoragePasswordKey(st.ID)
		keep[key] = true
		if p, ok := in.Passwords[st.ID]; ok && p != "" {
			if !stack.StoragePasswordOK(p) {
				fail(w, http.StatusBadRequest, "the password for "+st.ID+" must not contain a comma")
				return
			}
			sec.Set(key, p)
		}
	}
	// Passwords of removed shares are not kept around.
	for _, key := range sec.Keys() {
		if strings.HasPrefix(key, "STORAGE_") && strings.HasSuffix(key, "_PASSWORD") && !keep[key] {
			sec.Delete(key)
		}
	}
	if err := sec.Save(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := cfg.Save(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func friendlyStorageError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "there is no storage") {
		return msg + "; move the area elsewhere before removing its storage"
	}
	return msg
}

// handleStorageCheck tests a storage as entered, before it is saved.
func (s *Server) handleStorageCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Storage  stack.Storage `json:"storage"`
		Password string        `json:"password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	cfg, err := stack.Load(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	sec, err := stack.LoadSecrets(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := in.Storage
	if st.ID == "" {
		st.ID = "check"
	}
	// Validate it on its own, as the only storage.
	probe := *cfg
	probe.Storage, probe.Areas = []stack.Storage{st}, nil
	if err := probe.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if st.Type == stack.StorageFolder {
		if fi, err := os.Stat(st.Path); err != nil || !fi.IsDir() {
			fail(w, http.StatusBadRequest, fmt.Sprintf("%s does not exist on this machine; is the drive connected and mounted?", st.Path))
			return
		}
	}
	if in.Password != "" && !stack.StoragePasswordOK(in.Password) {
		fail(w, http.StatusBadRequest, "the password must not contain a comma")
		return
	}
	res, err := s.CheckStorage(cfg, sec, &st, in.Password)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, res)
}
