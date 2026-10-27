package daemon

import (
	"net"
	"strconv"
	"strings"

	"github.com/carsteneu/yesmem/internal/config"
)

// proxyForwardPort extracts the TCP port a sandboxed agent has to reach on the
// host loopback to talk to the yesmem proxy. ai-jail 2.x gives the sandbox its
// own loopback, so without a forward a sandboxed claude/codex cannot reach the
// proxy the host already points them at — 0.9.x shared the host loopback and
// needed nothing.
//
// Accepts ":9099", "127.0.0.1:9099", "localhost:9099" and a bare "9099". An
// unparseable address yields 0, which means "do not forward": forwarding a port
// we guessed would be worse than leaving the job without egress. Note that the
// agent side is configured with a fixed localhost:9099 (see internal/setup), so
// moving proxy.listen without updating that leaves agent runs without a reachable
// proxy.
func proxyForwardPort(listen string) int {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		listen = config.DefaultProxyListen
	}

	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		port = listen
	}

	n, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}
