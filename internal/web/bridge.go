package web

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"stackwire/internal/dockerx"
	"stackwire/internal/render"
	"stackwire/internal/stack"
)

// ServeProxyAccess keeps the web interface reachable for the proxy container
// while the stack has a domain. It blocks.
//
// The proxy reaches the host under the name host.docker.internal. On Linux
// that is the address of a Docker bridge on this machine, which containers
// can reach but a listener on 127.0.0.1 does not cover. So this listens on
// exactly that address: reachable from containers, not from the network.
// On Docker Desktop the name leads to the host's loopback interface and no
// extra listener is needed.
func ServeProxyAccess(dir string, handler http.Handler, log io.Writer) {
	var ln net.Listener
	current, lastErr := "", ""
	for {
		want := proxyAccessAddr(dir)
		if want != current {
			if ln != nil {
				ln.Close()
				ln, current = nil, ""
			}
			if want != "" {
				l, err := net.Listen("tcp", want)
				switch {
				case err == nil:
					ln, current, lastErr = l, want, ""
					go (&http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}).Serve(l)
					fmt.Fprintf(log, "Listening on %s for the proxy.\n", want)
				case err.Error() != lastErr:
					// Usually the main listener already covers this address.
					lastErr = err.Error()
					fmt.Fprintf(log, "Not listening on %s for the proxy: %v\n", want, err)
				}
			}
		}
		time.Sleep(15 * time.Second)
	}
}

// proxyAccessAddr returns the local address the proxy container uses to
// reach the host, or "" if there is none to listen on.
func proxyAccessAddr(dir string) string {
	cfg, err := stack.Load(dir)
	if err != nil || cfg.Domain == "" {
		return ""
	}
	// Ask the proxy container itself, so this matches whatever Docker is
	// configured to use as host gateway.
	out, err := dockerx.ProjectOutput(cfg.Name, "exec", "-T", render.ProxyService, "getent", "hosts", "host.docker.internal")
	if err != nil {
		return "" // proxy not running yet
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return ""
	}
	ip := net.ParseIP(fields[0])
	if ip == nil || ip.IsLoopback() || !isLocalIP(ip) {
		return ""
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(render.UIPort))
}

func isLocalIP(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}
