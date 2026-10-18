package ops

import (
	"strings"
	"testing"

	"stackwire/internal/catalog"
	"stackwire/internal/render"
)

func TestCheckPorts(t *testing.T) {
	apps := []catalog.App{{ID: "qbittorrent", Name: "qBittorrent"}, {ID: "sonarr", Name: "Sonarr"}}
	taken := map[int]bool{8080: true, 8081: true, 6881: true}
	free := func(port int, proto string) bool { return !taken[port] }

	ok := []render.HostPort{{Port: 8082, Proto: "tcp", App: "qbittorrent", Settable: true}, {Port: 8989, Proto: "tcp", App: "sonarr", Settable: true}}
	if err := checkPorts(ok, apps, free); err != nil {
		t.Errorf("free ports: %v", err)
	}

	err := checkPorts([]render.HostPort{
		{Port: 8080, Proto: "tcp", App: "qbittorrent", Settable: true},
		{Port: 6881, Proto: "udp", App: "qbittorrent"},
		{Port: 8082, Proto: "tcp", App: "sonarr", Settable: true},
	}, apps, free)
	if err == nil {
		t.Fatal("taken ports passed")
	}
	msg := err.Error()
	// 8081 is taken too and 8082 belongs to Sonarr, so 8083 is the suggestion.
	for _, want := range []string{"port 8080/tcp for qBittorrent is in use", "qbittorrent: 8083", "6881/udp for qBittorrent", "cannot be changed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}

	err = checkPorts([]render.HostPort{
		{Port: 9000, Proto: "tcp", App: "qbittorrent", Settable: true},
		{Port: 9000, Proto: "tcp", App: "sonarr", Settable: true},
	}, apps, free)
	if err == nil || !strings.Contains(err.Error(), "both qBittorrent and Sonarr") {
		t.Errorf("duplicate port: %v", err)
	}
}
