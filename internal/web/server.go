// Package web serves the browser interface for managing a stack.
package web

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"stackwire/internal/catalog"
	"stackwire/internal/dockerx"
	"stackwire/internal/ops"
	"stackwire/internal/render"
	"stackwire/internal/stack"
	"stackwire/internal/wire"
)

//go:embed static/index.html
var static embed.FS

const (
	sessionCookie = "stackwire_session"
	sessionTTL    = 12 * time.Hour

	maxLoginFailures = 10
	// Browsers only send this header from same-origin scripts, so its
	// presence rules out forged cross-site requests.
	csrfHeader = "X-Stackwire"
)

// Server is the web interface for the stack in one directory.
type Server struct {
	dir string

	// Containers reports the state of each compose service by name. Replaced in tests.
	Containers func(project string) (map[string]string, error)
	// ApplyTimeout is how long an apply waits for the apps to become ready.
	ApplyTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]time.Time
	failed   []time.Time // recent failed logins
	job      *job
	jobSeq   int
}

func New(dir string) *Server {
	return &Server{dir: dir, Containers: composeStates, ApplyTimeout: 8 * time.Minute, sessions: map[string]time.Time{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := static.ReadFile("static/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; frame-ancestors 'none'")
		w.Write(b)
	})
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/state", s.auth(s.handleState))
	mux.HandleFunc("PUT /api/config", s.auth(s.handleConfig))
	mux.HandleFunc("POST /api/password", s.auth(s.handlePassword))
	mux.HandleFunc("POST /api/apply", s.auth(s.handleApply))
	mux.HandleFunc("POST /api/stop", s.auth(s.handleStop))
	mux.HandleFunc("POST /api/apps/{id}/install", s.auth(s.handleSelect(true)))
	mux.HandleFunc("POST /api/apps/{id}/remove", s.auth(s.handleSelect(false)))
	mux.HandleFunc("POST /api/apps/{id}/restart", s.auth(s.handleRestart))
	mux.HandleFunc("GET /api/apps/{id}/logs", s.auth(s.handleLogs))
	return mux
}

// --- sessions ---

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) == "" {
			fail(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.validSession(c.Value) {
			fail(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next(w, r)
	}
}

func (s *Server) validSession(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[token]
	if !ok || time.Now().After(exp) {
		delete(s.sessions, token)
		return false
	}
	return true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ User, Password string }
	if r.Header.Get(csrfHeader) == "" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	if s.tooManyFailures() {
		fail(w, http.StatusTooManyRequests, "too many failed attempts, try again in a minute")
		return
	}
	st, err := ops.Load(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(in.User), []byte(st.Secrets.Get(stack.AdminUser)))
	passOK := subtle.ConstantTimeCompare([]byte(in.Password), []byte(st.Secrets.Get(stack.AdminPassword)))
	if userOK&passOK != 1 {
		s.mu.Lock()
		s.failed = append(s.failed, time.Now())
		s.mu.Unlock()
		time.Sleep(time.Second) // slows down guessing
		fail(w, http.StatusUnauthorized, "wrong user or password")
		return
	}
	token := rand.Text()
	s.mu.Lock()
	for t, exp := range s.sessions {
		if time.Now().After(exp) {
			delete(s.sessions, t)
		}
	}
	s.sessions[token] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: int(sessionTTL.Seconds()),
	})
	writeJSON(w, map[string]bool{"ok": true})
}

// tooManyFailures limits password guessing once the page is reachable from
// the internet: after maxLoginFailures failed logins within a minute, further
// attempts are refused until the oldest one is a minute old.
func (s *Server) tooManyFailures() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := time.Now().Add(-time.Minute)
	i := 0
	for i < len(s.failed) && s.failed[i].Before(cut) {
		i++
	}
	s.failed = s.failed[i:]
	return len(s.failed) >= maxLoginFailures
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

// --- state ---

type appState struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Selected    bool   `json:"selected"`  // part of the configuration
	Container   string `json:"container"` // compose state, empty if there is no container
	Healthy     bool   `json:"healthy"`
	URL         string `json:"url"`  // set in domain mode
	Port        int    `json:"port"` // published web port
	DefaultPort int    `json:"defaultPort"`
}

type stateResponse struct {
	Config struct {
		Domain   string `json:"domain"`
		Email    string `json:"email"`
		Timezone string `json:"timezone"`
		DataDir  string `json:"dataDir"`
		Dir      string `json:"dir"`
	} `json:"config"`
	AdminUser   string     `json:"adminUser"`
	UIURL       string     `json:"uiUrl"` // address of this page in domain mode
	Apps        []appState `json:"apps"`
	Pending     bool       `json:"pending"` // configuration and running stack differ
	DockerError string     `json:"dockerError,omitempty"`
	Job         jobView    `json:"job"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
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
	var resp stateResponse
	resp.Config.Domain, resp.Config.Email = st.Cfg.Domain, st.Cfg.Email
	resp.Config.Timezone, resp.Config.DataDir, resp.Config.Dir = st.Cfg.Timezone, st.Cfg.DataDir, st.Cfg.Dir
	resp.AdminUser = st.Secrets.Get(stack.AdminUser)
	resp.UIURL = render.UIURL(st.Cfg)

	containers, err := s.Containers(st.Cfg.Name)
	if err != nil {
		resp.DockerError = err.Error()
	}
	selected := map[string]bool{}
	for _, a := range st.Apps {
		selected[a.ID] = true
	}
	ctx := wire.NewCtx(st.Cfg, st.Secrets, st.Apps, io.Discard)
	ctx.HTTP = &http.Client{Timeout: 2 * time.Second}
	resp.Apps = make([]appState, len(all))
	var wg sync.WaitGroup
	for i, a := range all {
		as := appState{
			ID: a.ID, Name: a.Name, Description: a.Description, Category: a.Category,
			Selected: selected[a.ID], Container: containers[a.ID],
			Port: st.Cfg.HostPort(a.ID, a.Port), DefaultPort: a.Port,
		}
		if st.Cfg.Domain != "" {
			as.URL = st.Cfg.PublicURL(a.ID, a.Subdomain, a.Port)
		}
		if as.Selected != (as.Container == "running") {
			resp.Pending = true
		}
		resp.Apps[i] = as
		if as.Container == "running" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp.Apps[i].Healthy = wire.Healthy(ctx, a)
			}()
		}
	}
	wg.Wait()
	if !render.UpToDate(st.Cfg, st.Apps) {
		resp.Pending = true
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	jobID, _ := strconv.Atoi(r.URL.Query().Get("job"))
	resp.Job = s.jobView(jobID, offset)
	writeJSON(w, resp)
}

// composeStates asks docker compose for the state of each service.
func composeStates(project string) (map[string]string, error) {
	out, err := dockerx.ProjectOutput(project, "ps", "-a", "--format", "json")
	if err != nil {
		return nil, err
	}
	type entry struct{ Service, State string }
	states := map[string]string{}
	out = bytes.TrimSpace(out)
	// Older compose versions print one JSON array, newer ones one object per line.
	if bytes.HasPrefix(out, []byte("[")) {
		var list []entry
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, err
		}
		for _, e := range list {
			states[e.Service] = e.State
		}
		return states, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e entry
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		states[e.Service] = e.State
	}
	return states, sc.Err()
}

// --- changes ---

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Domain, Email, Timezone, DataDir string
		Ports                            map[string]int
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	s.edit(w, func(cfg *stack.Config) error {
		cfg.Domain = strings.ToLower(strings.TrimSpace(in.Domain))
		cfg.Email = strings.TrimSpace(in.Email)
		cfg.Timezone = strings.TrimSpace(in.Timezone)
		cfg.DataDir = strings.TrimSpace(in.DataDir)
		all, err := catalog.All()
		if err != nil {
			return err
		}
		cfg.Ports = map[string]int{}
		used := map[int]string{}
		for _, a := range all {
			p := a.Port
			// Only store ports that differ from the app's default.
			if v := in.Ports[a.ID]; v != 0 && v != a.Port {
				cfg.Ports[a.ID] = v
				p = v
			}
			if other, ok := used[p]; ok {
				return fmt.Errorf("port %d is used by both %s and %s", p, other, a.ID)
			}
			used[p] = a.ID
		}
		return nil
	})
}

func (s *Server) handleSelect(install bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := catalog.Resolve([]string{id}); err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		s.edit(w, func(cfg *stack.Config) error {
			var apps []string
			for _, a := range cfg.Apps {
				if a != id {
					apps = append(apps, a)
				}
			}
			if install {
				apps = append(apps, id)
			}
			cfg.Apps = apps
			return nil
		})
	}
}

// edit changes stackwire.yaml. Nothing is started; that is what apply is for.
func (s *Server) edit(w http.ResponseWriter, change func(*stack.Config) error) {
	if s.jobRunning() {
		fail(w, http.StatusConflict, "wait until the running task has finished")
		return
	}
	cfg, err := stack.Load(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err = change(cfg); err == nil {
		err = cfg.Validate()
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := cfg.Save(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	st, err := ops.Load(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]string{"password": st.Secrets.Get(stack.AdminPassword)})
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	s.startJob(w, "Applying changes", func(out io.Writer) error {
		st, err := ops.Load(s.dir)
		if err != nil {
			return err
		}
		return ops.Up(st, out, s.ApplyTimeout)
	})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.startJob(w, "Stopping all apps", func(out io.Writer) error {
		st, err := ops.Load(s.dir)
		if err != nil {
			return err
		}
		return ops.Down(st, out)
	})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	apps, err := catalog.Resolve([]string{r.PathValue("id")})
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	s.startJob(w, "Restarting "+apps[0].Name, func(out io.Writer) error {
		cfg, err := stack.Load(s.dir)
		if err != nil {
			return err
		}
		return dockerx.Project(cfg.Name, out, "restart", apps[0].ID)
	})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	apps, err := catalog.Resolve([]string{r.PathValue("id")})
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	cfg, err := stack.Load(s.dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var buf bytes.Buffer
	if err := dockerx.Project(cfg.Name, &buf, "logs", "--no-color", "--no-log-prefix", "--tail", "300", apps[0].ID); err != nil {
		fail(w, http.StatusInternalServerError, strings.TrimSpace(buf.String()+"\n"+err.Error()))
		return
	}
	writeJSON(w, map[string]string{"log": buf.String()})
}

// --- jobs ---

// job is one long-running task. Only one runs at a time, because they all
// change the same containers.
type job struct {
	id   int
	name string

	mu      sync.Mutex
	log     bytes.Buffer
	running bool
	err     string
}

func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.log.Write(p)
}

type jobView struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
	Log     string `json:"log"`    // output from the requested offset on
	Offset  int    `json:"offset"` // where the next request continues
}

func (s *Server) jobRunning() bool {
	s.mu.Lock()
	j := s.job
	s.mu.Unlock()
	if j == nil {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}

// jobView returns the current job. The log starts at offset if the client
// already follows this job, and at the beginning otherwise.
func (s *Server) jobView(clientJob, offset int) jobView {
	s.mu.Lock()
	j := s.job
	s.mu.Unlock()
	if j == nil {
		return jobView{}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if clientJob != j.id || offset < 0 || offset > j.log.Len() {
		offset = 0
	}
	return jobView{ID: j.id, Name: j.name, Running: j.running, Error: j.err,
		Log: string(j.log.Bytes()[offset:]), Offset: j.log.Len()}
}

func (s *Server) startJob(w http.ResponseWriter, name string, fn func(io.Writer) error) {
	s.mu.Lock()
	if s.job != nil {
		s.job.mu.Lock()
		busy := s.job.running
		s.job.mu.Unlock()
		if busy {
			s.mu.Unlock()
			fail(w, http.StatusConflict, "another task is still running: "+s.job.name)
			return
		}
	}
	s.jobSeq++
	j := &job{id: s.jobSeq, name: name, running: true}
	s.job = j
	s.mu.Unlock()

	go func() {
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("internal error: %v", p)
				}
			}()
			return fn(j)
		}()
		j.mu.Lock()
		defer j.mu.Unlock()
		if err != nil {
			j.err = err.Error()
			fmt.Fprintf(&j.log, "\nerror: %v\n", err)
		}
		j.running = false
	}()
	writeJSON(w, map[string]int{"job": j.id})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
