package proxy

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// peerPIDEntry maps a loopback peer endpoint to its owning process. The inode
// field is the validation anchor: if the cached inode no longer resolves, the
// entry is stale (port reuse / reconnect) and gets re-resolved.
type peerPIDEntry struct {
	pid   int
	inode uint64
	seen  time.Time
}

// peerPIDCache holds resolved peer endpoints for the proxy's loopback clients.
// OpenCode keeps persistent keep-alive connections, so entries are long-lived;
// prune() bounds the map against port recycling noise.
type peerPIDCache struct {
	mu sync.Mutex
	m  map[string]peerPIDEntry
}

func newPeerPIDCache() *peerPIDCache {
	return &peerPIDCache{m: make(map[string]peerPIDEntry)}
}

func (c *peerPIDCache) prune() {
	cutoff := time.Now().Add(-2 * time.Hour)
	c.mu.Lock()
	for k, e := range c.m {
		if e.seen.Before(cutoff) {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}

// peerSessionPID resolves the process that owns the CLIENT side of a proxy
// connection. For an accepted connection with peer endpoint peerPort, the
// client's own socket is the mirrored row local=peerPort rem=proxyPort
// (ESTABLISHED) in /proc/net/tcp{,6}; its inode maps to the client PID via
// /proc/<pid>/fd. Returns 0 when the lookup fails — callers must treat 0 as
// "no registration".
func peerSessionPID(listenAddr, remoteAddr string) int {
	listenPort := parsePort(listenAddr)
	if listenPort <= 0 {
		return 0
	}
	peerPort := parsePort(remoteAddr)
	if peerPort <= 0 {
		return 0
	}
	cacheKey := remoteAddr

	defaultPeerPIDs.mu.Lock()
	if e, ok := defaultPeerPIDs.m[cacheKey]; ok {
		defaultPeerPIDs.mu.Unlock()
		if e.pid > 0 && dirHasSocketFD(e.pid, socketTarget(e.inode)) {
			return e.pid
		}
	} else {
		defaultPeerPIDs.mu.Unlock()
	}

	inode := peerClientSocketInode(peerPort, listenPort)
	if inode == 0 {
		return 0
	}
	pid := findPIDBySocketInode(inode)
	defaultPeerPIDs.mu.Lock()
	defaultPeerPIDs.m[cacheKey] = peerPIDEntry{pid: pid, inode: inode, seen: time.Now()}
	defaultPeerPIDs.mu.Unlock()
	return pid
}

var defaultPeerPIDs = newPeerPIDCache()

func socketTarget(inode uint64) string {
	return "socket:[" + strconv.FormatUint(inode, 10) + "]"
}

func parsePort(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

// peerClientSocketInode scans /proc/net/tcp{,6} for the client-side row of the
// connection: local=peerPort rem=proxyPort, ESTABLISHED. The inode belongs to
// the opencode process (its outgoing socket fd), not to our accepted socket.
func peerClientSocketInode(peerPort, listenPort int) uint64 {
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if i == 0 || !strings.Contains(line, ":") {
				continue // header / malformed
			}
			f := strings.Fields(line)
			// sl local_address rem_address st tx:rx ... inode
			if len(f) < 10 || f[3] != "01" { // 01 = TCP_ESTABLISHED
				continue
			}
			localPort, err := strconv.ParseUint(afterColon(f[1]), 16, 16)
			if err != nil || int(localPort) != peerPort {
				continue
			}
			remPort, err := strconv.ParseUint(afterColon(f[2]), 16, 16)
			if err != nil || int(remPort) != listenPort {
				continue
			}
			inode, err := strconv.ParseUint(f[9], 10, 64)
			if err != nil || inode == 0 {
				continue
			}
			return inode
		}
	}
	return 0
}

// findPIDBySocketInode scans /proc for the process holding the socket fd.
func findPIDBySocketInode(inode uint64) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if dirHasSocketFD(pid, socketTarget(inode)) {
			return pid
		}
	}
	return 0
}

func dirHasSocketFD(pid int, target string) bool {
	fds, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/fd")
	if err != nil {
		return false
	}
	for _, fd := range fds {
		link, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/" + fd.Name())
		if err == nil && link == target {
			return true
		}
	}
	return false
}

func afterColon(s string) string {
	_, rest, ok := strings.Cut(s, ":")
	if !ok {
		return s
	}
	return rest
}
