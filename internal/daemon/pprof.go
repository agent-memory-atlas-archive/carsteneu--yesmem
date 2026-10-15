package daemon

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
)

// maxPprofSeconds caps the seconds parameter of the CPU profile and the
// execution trace. pprof only replaces a value that is <= 0 and the server sets
// no WriteTimeout, so without a cap a single request can pin a core for as long
// as it likes. Variable rather than constant so a test can observe the cap
// without waiting for it.
var maxPprofSeconds = 120

// StartPprofListener starts a loopback-only net/http/pprof endpoint and returns
// the server (for Shutdown) and the bound address (e.g. "127.0.0.1:7071").
//
// The profiles expose allocation sites, goroutine stacks and CPU samples — not
// session transcripts — so the endpoint answers "is this retained heap or
// indexing churn?" without leaking memory content. Three independent controls
// keep it from being driven by something other than a local operator: the
// listener is bound to loopback (resolvePprofListen rejects a non-loopback
// host), every request must carry a loopback Host header (loopbackHostOnly),
// which also stops a page running on this machine from reaching the port
// through DNS rebinding, and browser requests from another origin are refused
// (pprofGuard) so a page cannot start a profile with a plain GET.
//
// The handlers are mounted on their own mux, so they are not part of the
// daemon's HTTP API. Importing net/http/pprof also registers handlers on
// http.DefaultServeMux as a side effect of the package's init; the daemon never
// serves that mux.
func StartPprofListener(addr string) (*http.Server, string, error) {
	listen, err := resolvePprofListen(addr)
	if err != nil {
		return nil, "", err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, "", err
	}
	srv := &http.Server{
		Handler: loopbackHostOnly(pprofGuard(pprofMux())),
		// No WriteTimeout: CPU profiles and traces legitimately run for tens of
		// seconds. ReadHeaderTimeout and IdleTimeout still bound half-open and
		// idle connections, and shutdownServer bounds shutdown.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("pprof server error: %v", err)
		}
	}()
	bound := ln.Addr().String()
	log.Printf("pprof diagnostics ready on http://%s/debug/pprof/", bound)
	return srv, bound, nil
}

// startPprofIfEnabled starts the endpoint when enabled and returns a nil server
// otherwise. Seam so that "disabled starts nothing" is testable without running
// the whole daemon.
func startPprofIfEnabled(enabled bool, listen string) (*http.Server, error) {
	if !enabled {
		return nil, nil
	}
	srv, _, err := StartPprofListener(listen)
	return srv, err
}

// shutdownServer stops srv, giving in-flight requests a moment before closing
// connections, so a stalled profile stream cannot block daemon shutdown.
func shutdownServer(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		srv.Close()
	}
}

// pprofMux mounts the net/http/pprof handlers on a dedicated mux, GET-only.
// pprof.Index also serves the named runtime profiles (heap, goroutine, allocs,
// block, mutex, threadcreate) below /debug/pprof/.
//
// GET-only is not what keeps a browser out: an <img> tag or a GET form drives a
// plain GET with a valid Host header, and GET is enough to start a CPU profile
// or a trace. pprofGuard closes that path.
func pprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return mux
}

// pprofGuard rejects requests that a browser marks as coming from somewhere
// other than this origin, and caps the duration of the CPU profile and the
// execution trace.
//
// Browsers: a page in any browser on this machine can point an <img> or a
// fetch at the loopback port without a CORS preflight, so the request arrives
// with a valid Host header. Browsers label the requests they make with
// Sec-Fetch-Site and mark an Origin on the ones that can cross an origin, and
// those labels are what distinguishes a page from an operator running go tool
// pprof locally. A request that names an origin, or that labels itself cross-
// site or same-site, is refused: the endpoint serves exactly one origin, so a
// page on this host at another port ("same-site" — the site is scheme plus
// registrable domain, the port is not part of it) has no business here either.
func pprofGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isForeignBrowserRequest(r) {
			http.Error(w, "pprof: cross-site requests are rejected", http.StatusForbidden)
			return
		}
		// Both profile handlers run for as long as seconds asks, and this server
		// sets no WriteTimeout, so the value is capped here. NaN is caught
		// explicitly: "NaN > cap" is false, and what a float-to-int conversion of
		// NaN does is implementation-defined, so the guard must not let it through.
		if q := r.URL.Query(); q.Has("seconds") {
			if sec, err := strconv.ParseFloat(q.Get("seconds"), 64); err == nil && (math.IsNaN(sec) || sec > float64(maxPprofSeconds)) {
				q.Set("seconds", strconv.Itoa(maxPprofSeconds))
				r.URL.RawQuery = q.Encode()
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isForeignBrowserRequest reports whether a browser attached fetch metadata
// naming an origin other than this one. A request without those headers (curl,
// go tool pprof, an address-bar navigation) is not foreign.
func isForeignBrowserRequest(r *http.Request) bool {
	if r.Header.Get("Origin") != "" {
		return true
	}
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "cross-site", "same-site":
		return true
	}
	return false
}

// loopbackHostOnly rejects requests whose Host header is not a loopback name.
func loopbackHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "pprof: requests must use a loopback Host header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether an HTTP Host value names this machine's
// loopback interface, with or without a port and with or without IPv6 brackets.
func isLoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// resolvePprofListen normalizes addr to a loopback host:port. An empty address,
// a bare port or "localhost" falls back to 127.0.0.1. An explicit non-loopback
// host (0.0.0.0, ::, a LAN address) is rejected so pprof is never exposed.
func resolvePprofListen(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return config.DefaultPprofListen, nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if _, perr := strconv.Atoi(addr); perr == nil {
			return net.JoinHostPort("127.0.0.1", addr), nil
		}
		if strings.Contains(addr, ":") {
			return "", fmt.Errorf("invalid pprof listen address %q: use [host]:port for IPv6 (%w)", addr, err)
		}
		return "", fmt.Errorf("invalid pprof listen address %q: %w", addr, err)
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("pprof listen address %q is not loopback — refusing to expose diagnostics", addr)
	}
	return net.JoinHostPort(host, port), nil
}
