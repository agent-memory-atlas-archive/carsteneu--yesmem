package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResolvePprofListen(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "empty falls back to default", in: "", want: "127.0.0.1:7071"},
		{name: "explicit loopback", in: "127.0.0.1:7071", want: "127.0.0.1:7071"},
		{name: "bare port", in: "7071", want: "127.0.0.1:7071"},
		{name: "empty host", in: ":7071", want: "127.0.0.1:7071"},
		{name: "localhost", in: "localhost:7071", want: "127.0.0.1:7071"},
		{name: "ipv6 loopback", in: "[::1]:7071", want: "[::1]:7071"},
		{name: "port zero", in: "127.0.0.1:0", want: "127.0.0.1:0"},
		{name: "wildcard rejected", in: "0.0.0.0:7071", wantErr: true},
		{name: "ipv6 unspecified rejected", in: "[::]:7071", wantErr: true},
		{name: "lan address rejected", in: "192.168.1.5:7071", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePprofListen(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolvePprofListen(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePprofListen(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("resolvePprofListen(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStartPprofListenerRejectsNonLoopback(t *testing.T) {
	if _, _, err := StartPprofListener("0.0.0.0:0"); err == nil {
		t.Fatal("expected error for non-loopback pprof listen address")
	}
}

func TestStartPprofListenerServesLoopbackOnly(t *testing.T) {
	srv, addr, err := StartPprofListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("StartPprofListener: %v", err)
	}
	defer srv.Shutdown(context.Background())

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("bound address %q is not host:port: %v", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("listener bound to %q, want a loopback address", addr)
	}

	client := &http.Client{Timeout: 5 * time.Second}

	// Index page lists the available profiles.
	resp, err := client.Get("http://" + addr + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET /debug/pprof/: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /debug/pprof/ status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "heap") || !strings.Contains(string(body), "goroutine") {
		t.Errorf("index page does not list profiles, got %.120q", body)
	}

	// A named profile resolves through the index handler.
	resp, err = client.Get("http://" + addr + "/debug/pprof/heap?debug=1")
	if err != nil {
		t.Fatalf("GET /debug/pprof/heap: %v", err)
	}
	heapBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /debug/pprof/heap status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(heapBody), "heap profile") {
		t.Errorf("heap endpoint did not return a heap profile, got %.120q", heapBody)
	}

	// Only pprof paths are mounted on the dedicated mux.
	for _, path := range []string{"/", "/health", "/api/rpc/x"} {
		resp, err = client.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404 (pprof must not expose other paths)", path, resp.StatusCode)
		}
	}
}

func TestStartPprofIfEnabledDisabledStartsNothing(t *testing.T) {
	srv, err := startPprofIfEnabled(false, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startPprofIfEnabled(false): %v", err)
	}
	if srv != nil {
		t.Fatal("disabled diagnostics must not start a server")
	}
}

func TestStartPprofIfEnabledStartsWhenEnabled(t *testing.T) {
	srv, err := startPprofIfEnabled(true, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startPprofIfEnabled(true): %v", err)
	}
	if srv == nil {
		t.Fatal("enabled diagnostics must start a server")
	}
	defer shutdownServer(srv)
}

func TestPprofRejectsNonLoopbackHost(t *testing.T) {
	srv, addr, err := StartPprofListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("StartPprofListener: %v", err)
	}
	defer shutdownServer(srv)

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/debug/pprof/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "evil.example.com" // DNS-rebinding style request
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET with hostile Host: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("hostile Host status = %d, want 403", resp.StatusCode)
	}
}

func TestPprofRejectsNonGetMethods(t *testing.T) {
	srv, addr, err := StartPprofListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("StartPprofListener: %v", err)
	}
	defer shutdownServer(srv)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequest(method, "http://"+addr+"/debug/pprof/profile", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /debug/pprof/profile status = %d, want 405", method, resp.StatusCode)
		}
	}
}

// TestPprofRejectsForeignBrowserRequests covers the browser vector: the loopback
// Host check does not stop a page from firing a plain GET at the port — an <img
// src="http://127.0.0.1:7071/debug/pprof/trace?seconds=3600"> carries a valid
// Host header and would otherwise start an unbounded trace on the user's
// machine. same-site counts as foreign: a page served from another port on this
// host is a different origin, so it has no business here either.
func TestPprofRejectsForeignBrowserRequests(t *testing.T) {
	srv, addr, err := StartPprofListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("StartPprofListener: %v", err)
	}
	defer shutdownServer(srv)

	client := &http.Client{Timeout: 5 * time.Second}
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"origin header", map[string]string{"Origin": "https://evil.example.com"}, http.StatusForbidden},
		{"origin null", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"sec-fetch-site cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"sec-fetch-site same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"sec-fetch-site CROSS-SITE", map[string]string{"Sec-Fetch-Site": "CROSS-SITE"}, http.StatusForbidden},
		{"sec-fetch-site same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"sec-fetch-site none", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"no fetch metadata", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/debug/pprof/", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestPprofCapsSeconds is the end-to-end guard for the unbounded profile: the
// endpoint must not honour a seconds parameter above the cap. maxPprofSeconds is
// lowered so the test observes the cap instead of waiting for it.
func TestPprofCapsSeconds(t *testing.T) {
	origMax := maxPprofSeconds
	maxPprofSeconds = 1
	defer func() { maxPprofSeconds = origMax }()

	srv, addr, err := StartPprofListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("StartPprofListener: %v", err)
	}
	defer shutdownServer(srv)

	start := time.Now()
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get("http://" + addr + "/debug/pprof/trace?seconds=3600")
	if err != nil {
		t.Fatalf("GET trace with seconds=3600: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trace status = %d, want 200", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("trace ran %s for seconds=3600, want it capped to %s", elapsed, time.Duration(maxPprofSeconds)*time.Second)
	}
}

// TestPprofGuardCapsSecondsParameter pins the rewrite itself across the value
// shapes ParseFloat accepts: the handler must never see a value above the cap,
// non-finite ones included ("NaN > cap" is false).
func TestPprofGuardCapsSecondsParameter(t *testing.T) {
	origMax := maxPprofSeconds
	maxPprofSeconds = 120
	defer func() { maxPprofSeconds = origMax }()

	var seen []string
	guard := pprofGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("seconds"))
	}))

	cases := []struct{ in, want string }{
		{"3600", "120"},
		{"3600.5", "120"},
		{"1e400", "1e400"}, // ErrRange: pprof falls back to its own default
		{"NaN", "120"},
		{"nan", "120"},
		{"%2BInf", "120"}, // a literal "+Inf" in a query decodes to " Inf" and fails to parse
		{"Infinity", "120"},
		{"120", "120"},
		{"60", "60"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, tc := range cases {
		seen = nil
		req := httptest.NewRequest(http.MethodGet, "/debug/pprof/trace?seconds="+tc.in, nil)
		guard.ServeHTTP(httptest.NewRecorder(), req)
		if len(seen) != 1 || seen[0] != tc.want {
			t.Errorf("seconds=%q reached the handler as %q, want %q", tc.in, seen, tc.want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{host: "127.0.0.1:7071", want: true},
		{host: "127.0.0.1", want: true},
		{host: "localhost:7071", want: true},
		{host: "LOCALHOST", want: true},
		{host: "[::1]:7071", want: true},
		{host: "[::1]", want: true},
		{host: "evil.example.com", want: false},
		{host: "evil.example.com:7071", want: false},
		{host: "192.168.1.5:7071", want: false},
		{host: "", want: false},
	}

	for _, tc := range cases {
		if got := isLoopbackHost(tc.host); got != tc.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
