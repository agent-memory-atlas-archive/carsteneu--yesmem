package parser

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/carsteneu/yesmem/internal/models"
)

// maxLineBytes bounds a single JSONL line. It matches the buffer ceiling the
// scanner-based parsers use (64MB). An over-long line aborts the scan instead
// of being parsed, so a pathological line cannot blow up the heap.
const maxLineBytes = 64 * 1024 * 1024

// errLineTooLong is returned when a single line exceeds maxLineBytes.
var errLineTooLong = fmt.Errorf("line exceeds %d bytes", maxLineBytes)

// TailResult is the outcome of an incremental parse of the appended tail of a
// session file. Meta carries only the fields observable in the tail (session
// identity, ended-at); callers keep the previously stored values for everything
// else.
type TailResult struct {
	Messages []models.Message
	Meta     *SessionMeta
	// NewOffset is the byte offset after the last newline-terminated line — the
	// only position an append may resume from.
	NewOffset int64
	// CompleteCount is how many messages the lines up to NewOffset produced,
	// counting from the caller's baseSeq. A trailing line that is not terminated
	// yet is parsed by a whole-file parse but is not counted here, because the
	// writer may still extend it.
	CompleteCount int
	// PrefixHash fingerprints the last prefixWindowBytes bytes directly before
	// NewOffset. It is taken from the bytes this parse actually read, so a rewrite
	// landing between the parse and the watermark write makes the next run's check
	// fail (and reindex fully) instead of silently accepting a stale offset. Only
	// that window is covered: a length-preserving edit further back does not
	// change the offsets and is therefore not noticed here.
	PrefixHash string
}

// prefixWindowBytes is how many bytes before the watermark the fingerprint
// covers. It bounds what an append can notice: a length-preserving edit further
// back leaves both this window and the offsets intact, so the append is accepted
// and the stored prefix keeps the old bytes (stale, but not misaligned). See
// fingerprintBytes in internal/indexer for why the window is not the whole
// prefix — verifying that would mean re-reading it on every append.
const prefixWindowBytes = 256

// prefixWindow tracks the trailing bytes of a scan so the prefix fingerprint can
// come from the parsed bytes rather than from a second look at the file.
type prefixWindow struct {
	buf []byte
}

// seed pre-loads the window with the bytes directly before a scan offset: a scan
// starting there only sees what it reads, but the fingerprint has to cover the
// same window that verification reads back. Failures are ignored — an unreadable
// prefix only means "no fingerprint", which callers treat as unusable.
func (w *prefixWindow) seed(f *os.File, offset int64) {
	if offset <= 0 {
		return
	}
	n := int64(prefixWindowBytes)
	if offset < n {
		n = offset
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset-n); err != nil {
		return
	}
	w.buf = buf
}

// add records consumed bytes, keeping only the trailing window.
func (w *prefixWindow) add(b []byte) {
	if len(b) >= prefixWindowBytes {
		w.buf = append(w.buf[:0], b[len(b)-prefixWindowBytes:]...)
		return
	}
	w.buf = append(w.buf, b...)
	if len(w.buf) > prefixWindowBytes {
		n := copy(w.buf, w.buf[len(w.buf)-prefixWindowBytes:])
		w.buf = w.buf[:n]
	}
}

// fingerprint returns the hex SHA-256 of the window.
func (w *prefixWindow) fingerprint() string {
	sum := sha256.Sum256(w.buf)
	return hex.EncodeToString(sum[:])
}

// lineSink consumes parsed lines and reports how many messages it produced.
type lineSink interface {
	consume(line []byte)
	count() int
}

// scanResult records how far a scan progressed. Consumed and Complete differ only
// when the input ended with an unterminated line: Consumed covers everything that
// was handed to the sink, Complete stops at the last newline.
type scanResult struct {
	Consumed      int64
	Complete      int64
	CompleteCount int
	PrefixHash    string
}

// lineReader reads newline-terminated lines while keeping an exact byte count
// of what it consumed, so offsets never drift. Fragments are assembled with
// ReadSlice, which caps memory at the reader's buffer size per call.
type lineReader struct {
	r   *bufio.Reader
	max int
}

// next returns the following line. A newline-terminated line comes back with a
// nil error and includes its terminator. When the input ends without a
// terminator the residual bytes are returned together with
// io.ErrUnexpectedEOF, so the caller can decide whether to parse them.
func (lr *lineReader) next() ([]byte, error) {
	var buf []byte
	for {
		frag, err := lr.r.ReadSlice('\n')
		if len(buf)+len(frag) > lr.max {
			// Drain the rest of the over-long line so the next call stays
			// aligned to a line boundary.
			for err == bufio.ErrBufferFull {
				_, err = lr.r.ReadSlice('\n')
			}
			return nil, errLineTooLong
		}
		buf = append(buf, frag...)
		switch {
		case err == nil:
			return buf, nil // fragment ended with '\n'
		case err == bufio.ErrBufferFull:
			continue // buffer filled before the delimiter
		default:
			return buf, io.ErrUnexpectedEOF
		}
	}
}

// scanSession invokes sink for every line in [offset, EOF) and reports how far it
// got. An unterminated trailing line is handed to the sink only when
// acceptTrailingLine is set — the whole-file parse does so, because that is what
// the historical scanner did; an append must not, because the writer may still
// extend it and only complete lines may advance the watermark.
func scanSession(path string, offset int64, acceptTrailingLine bool, sink lineSink) (scanResult, error) {
	res := scanResult{Consumed: offset, Complete: offset, CompleteCount: sink.count()}

	f, err := os.Open(path)
	if err != nil {
		return res, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return res, fmt.Errorf("seek %s: %w", path, err)
		}
	}

	lr := &lineReader{r: bufio.NewReaderSize(f, 256*1024), max: maxLineBytes}
	var window prefixWindow
	window.seed(f, offset)
	for {
		line, err := lr.next()
		switch {
		case err == io.EOF:
			res.PrefixHash = window.fingerprint()
			return res, nil
		case err == io.ErrUnexpectedEOF:
			// The window still ends at the last newline: an accepted trailing
			// fragment is parsed but must not move the fingerprint past Complete.
			res.PrefixHash = window.fingerprint()
			if acceptTrailingLine && len(line) > 0 {
				sink.consume(line)
				res.Consumed += int64(len(line))
			}
			return res, nil
		case err != nil:
			return res, fmt.Errorf("scan %s: %w", path, err)
		}
		sink.consume(line)
		res.Consumed += int64(len(line))
		res.Complete = res.Consumed
		res.CompleteCount = sink.count()
		window.add(line)
	}
}

// ParseAutoTail parses the byte range [offset, EOF) of a session file, emitting
// sequence numbers that continue at baseSeq (the number of messages already
// stored for the session, which the parsers' sequence-is-slice-index invariant
// makes exact).
//
// Offset 0 is a whole-file parse: an unterminated trailing line is accepted, the
// way the historical scanner accepted it. From any later offset there is a
// watermark to protect, so such a line is deferred until its terminator arrives.
// Persist TailResult.NewOffset to make the next append incremental.
func ParseAutoTail(path string, offset int64, baseSeq int) (*TailResult, error) {
	acceptTrailingLine := offset == 0

	if isCodexPath(path) {
		sink := newCodexSink(baseSeq)
		res, err := scanSession(path, offset, acceptTrailingLine, sink)
		if err != nil {
			return nil, err
		}
		if sink.meta.Project == "" {
			sink.meta.Project = projectFromCodexPath(path)
		}
		return &TailResult{
			Messages:      sink.messages,
			Meta:          sink.meta,
			NewOffset:     res.Complete,
			CompleteCount: res.CompleteCount,
			PrefixHash:    res.PrefixHash,
		}, nil
	}

	sink := newClaudeSink(baseSeq)
	res, err := scanSession(path, offset, acceptTrailingLine, sink)
	if err != nil {
		return nil, err
	}
	return &TailResult{
		Messages:      sink.messages,
		Meta:          sink.meta,
		NewOffset:     res.Complete,
		CompleteCount: res.CompleteCount,
		PrefixHash:    res.PrefixHash,
	}, nil
}

// identityPrefixLines bounds how many leading lines are searched for a session
// identity. Claude Code writes non-session header lines (file-history-snapshot)
// ahead of the first message, so line 1 alone is not enough — but the identity
// is always in the header block, never hundreds of lines in.
const identityPrefixLines = 32

// FileSessionIdentity reads the first line of a session file and returns the raw
// session identity it carries: the subagent id when present (subagent files
// carry the parent session id in sessionId), otherwise the session id; for Codex
// the id from the session_meta header. Callers normalize the result with
// models.NormalizeSessionID. An empty result means "identity unknown" and must
// be treated as "reindex fully".
//
// Up to identityPrefixLines lines are inspected because the first line(s) may
// carry no identity at all. The value returned matches what the full parser
// derives, as long as the identity appears within that prefix.
func FileSessionIdentity(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := &lineReader{r: bufio.NewReaderSize(f, 64*1024), max: maxLineBytes}
	var sessionID, agentID string
	for i := 0; i < identityPrefixLines; i++ {
		line, err := lr.next()
		if err != nil {
			// Empty file, or a head that is still being written: unknown.
			break
		}

		var rl rawLine
		if json.Unmarshal(line, &rl) == nil {
			if agentID == "" && rl.AgentID != "" {
				agentID = rl.AgentID
			}
			if sessionID == "" && rl.SessionID != "" {
				sessionID = rl.SessionID
			}
		}
		if sessionID == "" {
			var cl codexLine
			if json.Unmarshal(line, &cl) == nil && cl.Type == "session_meta" {
				var payload codexSessionMetaPayload
				if json.Unmarshal(cl.Payload, &payload) == nil && payload.ID != "" {
					sessionID = payload.ID
				}
			}
		}
		if agentID != "" && sessionID != "" {
			break
		}
	}

	if agentID != "" {
		return agentID, nil
	}
	return sessionID, nil
}
