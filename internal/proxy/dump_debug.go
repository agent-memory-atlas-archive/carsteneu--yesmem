package proxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Debug request dumps. Three sites write them — the pre- and post-pipeline body
// in proxy.go and the forwarded body in proxy_forward.go / openai_parity.go —
// and all three go through writeDebugDump so the names cannot drift apart.
//
// Name: <dataDir>/debug/req_<YYYYMMDD-HHMMSS.mmm>_<thread>_<reqIdx>_<kind>.json
// reqIdx is the process-local request counter and starts at 0 again after a
// proxy restart, and the previous scheme was second-granular, so dumps of a new
// process lifetime silently overwrote the old ones. Mode 0600: the bodies are
// full transcripts.
const (
	dumpDirName  = "debug"
	dumpKindPre  = "pre"
	dumpKindBody = "body"
	dumpKindPost = "post"

	// threadTokenLen keeps the thread id in the name short; the timestamp and
	// reqIdx already separate dumps within a process lifetime.
	threadTokenLen = 12

	// defaultDumpMaxFiles / defaultDumpMaxAge / defaultDumpMaxBytes bound the
	// directory: a debugging session writes megabytes per request (53 GB during
	// the PRX-P16 incident) and a single dump reached 49.9 MB, so a file count
	// alone is no disk bound. Override with YESMEM_PROXY_DEBUG_MAX_FILES,
	// YESMEM_PROXY_DEBUG_MAX_AGE and YESMEM_PROXY_DEBUG_MAX_BYTES.
	defaultDumpMaxFiles = 200
	defaultDumpMaxAge   = 24 * time.Hour
	defaultDumpMaxBytes = 1 << 30 // 1 GiB

	// dumpPruneInterval keeps the directory walk off the request path: while a
	// large pre-scheme backlog sits in the directory, the walk alone costs tens
	// of milliseconds.
	dumpPruneInterval = 2 * time.Second
)

// lastDumpPruneNano is the unix-nano timestamp of the last prune.
var lastDumpPruneNano atomic.Int64

// dumpNameRe matches exactly the names this file writes. The cleanup must never
// touch anything else: pre-scheme dumps in debug/ (req_<idx>_body.json), the
// dumps in logs/ and the logs/dumps-pre-deploy-* archives all have to survive.
var dumpNameRe = regexp.MustCompile(`^req_\d{8}-\d{6}\.\d{3}_[A-Za-z0-9.-]+_\d+_(?:pre|body|post)(?:-\d+)?\.json$`)

// debugDumpsEnabled reports whether YESMEM_PROXY_DEBUG=1 is set. Call sites use
// it to skip the marshalling work; writeDebugDump re-checks it.
func debugDumpsEnabled() bool { return os.Getenv("YESMEM_PROXY_DEBUG") == "1" }

// writeDebugDump writes one request body and prunes the dump directory.
// Returns the path written, or "" when dumps are off, dataDir is empty, or the
// write failed. Never overwrites: two dumps that land on the same millisecond
// name (a restarted process reusing reqIdx) get a "-2", "-3", … suffix.
func writeDebugDump(dataDir, threadID, kind string, reqIdx int, body []byte) string {
	if !debugDumpsEnabled() || dataDir == "" {
		return ""
	}
	dir := filepath.Join(dataDir, dumpDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return ""
	}
	name := strings.TrimSuffix(dumpFileName(time.Now(), threadID, kind, reqIdx), ".json")

	for i := 1; i <= 100; i++ {
		suffix := ""
		if i > 1 {
			suffix = fmt.Sprintf("-%d", i)
		}
		path := filepath.Join(dir, fmt.Sprintf("%s%s.json", name, suffix))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return ""
		}
		_, writeErr := f.Write(body)
		f.Close()
		if writeErr != nil {
			return ""
		}
		pruneDumpsIfDue(dir)
		return path
	}
	return ""
}

// pruneDumpsIfDue prunes the dump directory at most once per dumpPruneInterval.
func pruneDumpsIfDue(dir string) {
	now := time.Now().UnixNano()
	prev := lastDumpPruneNano.Load()
	if now-prev < int64(dumpPruneInterval) {
		return
	}
	if !lastDumpPruneNano.CompareAndSwap(prev, now) {
		return
	}
	pruneDebugDumps(dir, time.Now(), dumpMaxFiles(), dumpMaxAge(), dumpMaxBytes())
}

// dumpFileName builds a dump name: millisecond timestamp, filename-safe thread
// token, request index and kind.
func dumpFileName(now time.Time, threadID, kind string, reqIdx int) string {
	return fmt.Sprintf("req_%s_%s_%d_%s.json",
		now.Format("20060102-150405.000"), threadToken(threadID), reqIdx, kind)
}

// threadToken reduces a thread id to a short filename-safe token. Thread ids
// look like "opencode:ses_ebc14c08ffeX3thcK0w81q4OZ" or "sess-1759..."; the
// session part after the last ":" is what identifies the session, every
// character outside [A-Za-z0-9.-] becomes "-", and the result is truncated.
func threadToken(threadID string) string {
	token := threadID
	if i := strings.LastIndex(token, ":"); i >= 0 {
		token = token[i+1:]
	}
	var b strings.Builder
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	token = b.String()
	if len(token) > threadTokenLen {
		token = token[:threadTokenLen]
	}
	if token == "" {
		return "nothread"
	}
	return token
}

// pruneDebugDumps deletes dumps older than maxAge and everything beyond the
// newest maxFiles dumps, then trims the oldest entries until the remaining total
// is within maxBytes. A maxFiles, maxAge or maxBytes <= 0 disables that bound.
// Only files matching dumpNameRe are candidates.
func pruneDebugDumps(dir string, now time.Time, maxFiles int, maxAge time.Duration, maxBytes int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type dumpFile struct {
		name string
		size int64
		mod  time.Time
	}
	files := make([]dumpFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !dumpNameRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, dumpFile{name: e.Name(), size: info.Size(), mod: info.ModTime()})
	}
	// The name carries a millisecond timestamp, so lexical order is
	// chronological order: newest first.
	sort.Slice(files, func(i, j int) bool { return files[i].name > files[j].name })

	var total int64
	for i, f := range files {
		switch {
		case maxFiles > 0 && i >= maxFiles:
		case maxAge > 0 && now.Sub(f.mod) > maxAge:
		case maxBytes > 0 && total+f.size > maxBytes:
		default:
			total += f.size
			continue
		}
		os.Remove(filepath.Join(dir, f.name))
	}
}

func dumpMaxFiles() int {
	if v := os.Getenv("YESMEM_PROXY_DEBUG_MAX_FILES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultDumpMaxFiles
}

func dumpMaxAge() time.Duration {
	if v := os.Getenv("YESMEM_PROXY_DEBUG_MAX_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultDumpMaxAge
}

func dumpMaxBytes() int64 {
	if v := os.Getenv("YESMEM_PROXY_DEBUG_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultDumpMaxBytes
}
