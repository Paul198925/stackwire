package web

import (
	"bufio"
	"bytes"
	"encoding/json"
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
	"stackwire/internal/storage"
	"stackwire/internal/wire"
)

const (
	// `docker stats` takes about two seconds, so several open browser tabs
	// share one measurement.
	dashboardTTL = 4 * time.Second
	// Counting a library means fetching it, which is too heavy to do on every refresh.
	factsTTL = time.Minute
)

type dashboardApp struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	State    string      `json:"state"`  // compose state, e.g. running
	Uptime   string      `json:"uptime"` // as docker words it, e.g. "Up 3 hours"
	CPU      float64     `json:"cpu"`    // percent of one core
	Mem      uint64      `json:"mem"`    // bytes
	NetRx    uint64      `json:"netRx"`  // bytes since the container started
	NetTx    uint64      `json:"netTx"`
	Facts    []wire.Fact `json:"facts,omitempty"`
	FactsErr string      `json:"factsError,omitempty"`
}

type dashboardResponse struct {
	Host struct {
		CPUs     int    `json:"cpus"`
		MemTotal uint64 `json:"memTotal"`
		OS       string `json:"os"`
		Kernel   string `json:"kernel"`
		Docker   string `json:"docker"`
	} `json:"host"`
	Disk struct {
		Path  string `json:"path"`
		Total uint64 `json:"total"`
		Free  uint64 `json:"free"`
	} `json:"disk"`
	Apps        []dashboardApp `json:"apps"` // the selected apps
	Mode        string         `json:"mode"` // "domain" or "ports"
	Domain      string         `json:"domain"`
	Dir         string         `json:"dir"`
	DockerError string         `json:"dockerError,omitempty"`
	At          time.Time      `json:"at"`
}

type dashboardCache struct {
	mu    sync.Mutex
	at    time.Time
	resp  *dashboardResponse
	facts map[string]factsEntry
}

type factsEntry struct {
	at    time.Time
	facts []wire.Fact
	err   string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// One lock for the whole measurement: concurrent requests wait and then
	// get the fresh result instead of each running docker themselves.
	s.dash.mu.Lock()
	defer s.dash.mu.Unlock()
	if s.dash.resp != nil && time.Since(s.dash.at) < dashboardTTL {
		writeJSON(w, s.dash.resp)
		return
	}
	resp, err := s.measure()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.dash.resp, s.dash.at = resp, time.Now()
	writeJSON(w, resp)
}

func (s *Server) measure() (*dashboardResponse, error) {
	st, err := ops.Load(s.dir)
	if err != nil {
		return nil, err
	}
	resp := &dashboardResponse{Mode: "ports", Domain: st.Cfg.Domain, Dir: st.Cfg.Dir, At: time.Now()}
	if st.Cfg.Domain != "" {
		resp.Mode = "domain"
	}
	resp.Disk.Path = st.Cfg.Abs(st.Cfg.DataDir)
	// The data folder does not exist before the first apply; its parent is on the same disk.
	for _, p := range []string{resp.Disk.Path, st.Cfg.Dir} {
		if total, free, err := storage.Space(p); err == nil {
			resp.Disk.Total, resp.Disk.Free = total, free
			break
		}
	}

	if out, err := dockerx.Output("info", "--format", "{{json .}}"); err != nil {
		resp.DockerError = err.Error()
	} else {
		var info struct {
			NCPU            int
			MemTotal        uint64
			OperatingSystem string
			KernelVersion   string
			ServerVersion   string
		}
		if err := json.Unmarshal(out, &info); err == nil {
			resp.Host.CPUs, resp.Host.MemTotal = info.NCPU, info.MemTotal
			resp.Host.OS, resp.Host.Kernel, resp.Host.Docker = info.OperatingSystem, info.KernelVersion, info.ServerVersion
		}
	}

	containers := map[string]containerInfo{}
	if resp.DockerError == "" {
		if err := s.CheckProject(st.Cfg); err != nil {
			resp.DockerError = err.Error()
		} else if containers, err = composeContainers(st.Cfg.Name); err != nil {
			resp.DockerError = err.Error()
		}
	}
	var running []string
	for _, c := range containers {
		if c.State == "running" {
			running = append(running, c.Name)
		}
	}
	usage := map[string]containerUsage{}
	if len(running) > 0 {
		if usage, err = containerStats(running); err != nil && resp.DockerError == "" {
			resp.DockerError = err.Error()
		}
	}

	ctx := wire.NewCtx(st.Cfg, st.Secrets, st.Apps, io.Discard)
	ctx.HTTP = &http.Client{Timeout: 5 * time.Second}
	resp.Apps = make([]dashboardApp, len(st.Apps))
	var wg sync.WaitGroup
	for i, a := range st.Apps {
		c := containers[a.ID]
		// An app's usage includes its sidecars (database, cache, ...).
		var u containerUsage
		services := []string{a.ID}
		for _, sc := range a.Sidecars {
			services = append(services, render.SidecarService(a, sc))
		}
		for _, svc := range services {
			x := usage[containers[svc].Name]
			u.CPU, u.Mem, u.NetRx, u.NetTx = u.CPU+x.CPU, u.Mem+x.Mem, u.NetRx+x.NetRx, u.NetTx+x.NetTx
		}
		resp.Apps[i] = dashboardApp{ID: a.ID, Name: a.Name, State: c.State, Uptime: c.Status,
			CPU: u.CPU, Mem: u.Mem, NetRx: u.NetRx, NetTx: u.NetTx}
		if c.State != "running" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp.Apps[i].Facts, resp.Apps[i].FactsErr = s.facts(ctx, a)
		}()
	}
	wg.Wait()
	return resp, nil
}

// facts returns an app's numbers, from the cache while they are fresh.
// The caller holds s.dash.mu; the map itself needs its own guard because
// the apps are asked concurrently.
func (s *Server) facts(ctx *wire.Ctx, a catalog.App) ([]wire.Fact, string) {
	s.factsMu.Lock()
	e, ok := s.dash.facts[a.ID]
	s.factsMu.Unlock()
	if ok && time.Since(e.at) < factsTTL {
		return e.facts, e.err
	}
	facts, err := wire.Facts(ctx, a)
	e = factsEntry{at: time.Now(), facts: facts}
	if err != nil {
		e.err = err.Error()
		// An app that is still starting answers soon; do not hold the error for a minute.
		e.at = time.Now().Add(-factsTTL + 10*time.Second)
	}
	s.factsMu.Lock()
	if s.dash.facts == nil {
		s.dash.facts = map[string]factsEntry{}
	}
	s.dash.facts[a.ID] = e
	s.factsMu.Unlock()
	return e.facts, e.err
}

type containerInfo struct {
	Service string
	Name    string
	State   string
	Status  string
}

// composeContainers asks docker compose for the containers of a project, by service.
func composeContainers(project string) (map[string]containerInfo, error) {
	out, err := dockerx.ProjectOutput(project, "ps", "-a", "--format", "json")
	if err != nil {
		return nil, err
	}
	res := map[string]containerInfo{}
	out = bytes.TrimSpace(out)
	// Older compose versions print one JSON array, newer ones one object per line.
	if bytes.HasPrefix(out, []byte("[")) {
		var list []containerInfo
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, err
		}
		for _, c := range list {
			res[c.Service] = c
		}
		return res, nil
	}
	err = eachJSONLine(out, func(line []byte) error {
		var c containerInfo
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		res[c.Service] = c
		return nil
	})
	return res, err
}

// composeStates is the part of composeContainers the app list needs.
func composeStates(project string) (map[string]string, error) {
	containers, err := composeContainers(project)
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	for service, c := range containers {
		states[service] = c.State
	}
	return states, nil
}

type containerUsage struct {
	CPU          float64
	Mem          uint64
	NetRx, NetTx uint64
}

// containerStats takes one resource measurement of the named containers.
func containerStats(names []string) (map[string]containerUsage, error) {
	out, err := dockerx.Output(append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, names...)...)
	if err != nil {
		return nil, err
	}
	res := map[string]containerUsage{}
	err = eachJSONLine(out, func(line []byte) error {
		var s struct{ Name, CPUPerc, MemUsage, NetIO string }
		if err := json.Unmarshal(line, &s); err != nil {
			return err
		}
		var u containerUsage
		u.CPU, _ = strconv.ParseFloat(strings.TrimSuffix(s.CPUPerc, "%"), 64)
		used, _, _ := strings.Cut(s.MemUsage, "/")
		u.Mem = parseSize(used)
		rx, tx, _ := strings.Cut(s.NetIO, "/")
		u.NetRx, u.NetTx = parseSize(rx), parseSize(tx)
		res[s.Name] = u
		return nil
	})
	return res, err
}

func eachJSONLine(out []byte, fn func([]byte) error) error {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// parseSize reads a size as docker prints it, e.g. "123.4MiB" or "1.2kB".
// Unreadable input counts as 0.
func parseSize(s string) uint64 {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i < 0 {
		i = len(s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	units := map[string]float64{
		"": 1, "B": 1,
		"kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
	}
	mult, ok := units[strings.TrimSpace(s[i:])]
	if !ok {
		return 0
	}
	return uint64(n * mult)
}
