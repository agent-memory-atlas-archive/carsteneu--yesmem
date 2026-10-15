package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/bloom"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/parser"
	"github.com/carsteneu/yesmem/internal/storage"
)

// Indexer orchestrates the full indexing pipeline.
type Indexer struct {
	store          *storage.Store
	bloom          *bloom.Manager
	archiver       *archive.Archiver
	excludeProject map[string]bool

	// IndexSession is not reentrant: the watcher pool and the startup scan both
	// call it, and two passes over one file — or over two files carrying the same
	// session id — interleave their delete and insert into duplicates or into a
	// session missing rows. Both callers run inside one process, so in-process
	// locks are enough; the unique (session_id, sequence) index covers the case of
	// two daemons, where no lock can.
	pathLocks    keyedLocks
	sessionLocks keyedLocks
}

// New creates an indexer with all dependencies.
func New(store *storage.Store, bloomMgr *bloom.Manager, arch *archive.Archiver, excludeProjects []string) *Indexer {
	exclude := make(map[string]bool, len(excludeProjects))
	for _, p := range excludeProjects {
		exclude[strings.ToLower(p)] = true
	}
	return &Indexer{
		store:          store,
		bloom:          bloomMgr,
		archiver:       arch,
		excludeProject: exclude,
	}
}

// errNotAppendable reports that the incremental fast path does not apply to a
// file (no usable watermark, identity mismatch, shrinking file). The caller
// then reindexes fully.
var errNotAppendable = errors.New("append fast path not applicable")

// IndexSession runs the pipeline for a single session JSONL file.
//
// When the file only grew since the last run and still belongs to the same
// session, only the appended tail is parsed and inserted — a running session no
// longer costs O(file size) per write. Anything else (shrink, rewrite, missing
// or stale watermark) takes the full path, and so does a failed append: a
// growing file indexed correctly beats one indexed cheaply.
func (idx *Indexer) IndexSession(jsonlPath string) error {
	// One file, one pass. The pool deduplicates only the paths submitted through
	// it, and the startup scan reaches the same file through IndexAll. The key is
	// the cleaned path so that two spellings of one file ("./x" and "x") do not
	// silently end up in two different critical sections.
	unlock := idx.pathLocks.lock(filepath.Clean(jsonlPath))
	defer unlock()

	// 1. Check if reindex needed
	info, err := os.Stat(jsonlPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", jsonlPath, err)
	}
	if !idx.store.NeedsReindex(jsonlPath, info.Size(), info.ModTime()) {
		return nil // already up to date
	}

	// 2. Incremental fast path — appends only.
	if state := idx.appendWatermark(jsonlPath, info); state != nil {
		err := idx.indexAppendedTail(jsonlPath, info, state)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errNotAppendable) {
			log.Printf("warn: incremental index %q: %v — falling back to full reindex", jsonlPath, err)
		}
	}

	return idx.indexFullSession(jsonlPath, info)
}

// appendWatermark returns the stored watermark when the file qualifies for an
// append-only pass, or nil when the file needs a full parse. The check is purely
// local (no parsing) so a non-qualifying file costs one row read.
func (idx *Indexer) appendWatermark(jsonlPath string, info os.FileInfo) *models.IndexState {
	state, err := idx.store.GetIndexState(jsonlPath)
	if err != nil || state == nil {
		return nil // never indexed, or no watermark yet
	}
	if state.IndexedOffset <= 0 || state.IndexedSessionID == "" || state.IndexedMsgCount < 0 {
		return nil
	}
	// Only strict growth is an append. Equal size means an in-place rewrite and
	// a smaller size means truncation; both need a full pass.
	if info.Size() <= state.FileSize || state.IndexedOffset > info.Size() {
		return nil
	}
	// Every watermark is the position after a complete line, so the byte before
	// it must be a newline. If it is not, the file was rewritten in place to a
	// larger size and seeking there would land inside a line.
	if !offsetSitsOnLineBoundary(jsonlPath, state.IndexedOffset) {
		return nil
	}
	// Size and the boundary byte alone cannot tell an append from an in-place
	// rewrite of the same length. The bytes before the watermark can: an append
	// leaves them untouched.
	prefix, err := prefixFingerprint(jsonlPath, state.IndexedOffset)
	if err != nil || state.IndexedPrefixHash == "" || prefix != state.IndexedPrefixHash {
		return nil
	}
	return state
}

// fingerprintBytes is how many bytes preceding the watermark are hashed to
// check that the already indexed region was not rewritten in place. A few
// hundred bytes catch the realistic cases — a rewrite that shifts the line
// boundaries, a replaced tail — at a negligible read cost.
//
// It is a window at the watermark, not a hash of the whole prefix. A
// length-preserving edit further back (a secret redacted in hindsight, say)
// stays invisible: the offsets still line up, so the append is accepted and the
// changed bytes remain as they were indexed until a full pass happens for some
// other reason. Verifying a whole-prefix hash would mean re-reading the prefix on
// every append — the cost this fast path exists to avoid — so the narrow window
// is the deliberate trade: an append can never misalign, but the stored prefix
// is not guaranteed to be current.
const fingerprintBytes = 256

// prefixFingerprint hashes the bytes immediately preceding an append watermark.
// The value stored at index time is taken from the parse itself
// (TailResult.PrefixHash); this function re-reads the same window from disk to
// verify it before an append is allowed. Both cover the last up to
// prefixWindowBytes bytes before the offset.
func prefixFingerprint(path string, offset int64) (string, error) {
	if offset <= 0 {
		return "", nil
	}
	n := int64(fingerprintBytes)
	if offset < n {
		n = offset
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset-n); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// offsetSitsOnLineBoundary reports whether the byte preceding offset is a
// newline, i.e. whether offset is the start of a line.
func offsetSitsOnLineBoundary(path string, offset int64) bool {
	if offset <= 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var b [1]byte
	if _, err := f.ReadAt(b[:], offset-1); err != nil {
		return false
	}
	return b[0] == '\n'
}

// indexAppendedTail parses and stores only the bytes after the watermark,
// leaving everything already indexed untouched (no session-wide delete, so the
// FTS index grows incrementally as well).
func (idx *Indexer) indexAppendedTail(jsonlPath string, info os.FileInfo, state *models.IndexState) error {
	// The whole append runs under the session's lock. Its trim removes everything
	// past the watermark, so a full pass for another file of the same session must
	// not be interleaved with it — and the checks below (identity, ownership,
	// contiguity) only mean anything while the session's rows cannot change
	// underneath them. The tail parse is cheap, so holding the lock across it costs
	// nothing.
	unlock := idx.sessionLocks.lock(state.IndexedSessionID)
	defer unlock()

	existing, err := idx.store.GetSession(state.IndexedSessionID)
	if err != nil {
		return fmt.Errorf("%w: session %s: %v", errNotAppendable, state.IndexedSessionID, err)
	}

	// Mirrors the full path: an excluded project is not indexed at all, even when
	// the file already carries a watermark from before it was excluded.
	if idx.excludeProject[strings.ToLower(existing.Project)] || idx.excludeProject[existing.ProjectShort] {
		return nil
	}

	// The path may have been reused for another session (rotation, import
	// rewrite). Verify the head against the watermark before trusting offsets.
	identity, err := parser.FileSessionIdentity(jsonlPath)
	if err != nil {
		return err
	}
	if identity == "" || models.NormalizeSessionID(existing.SourceAgent, identity) != state.IndexedSessionID {
		return fmt.Errorf("%w: head identity %q does not match watermark %q",
			errNotAppendable, identity, state.IndexedSessionID)
	}

	// Another file carrying the same session id would lose messages to this
	// append (the trim below removes everything past this file's watermark), so
	// only the owning file may append.
	elsewhere, err := idx.store.SessionIndexedByOtherPath(existing.ID, jsonlPath)
	if err != nil {
		return err
	}
	if elsewhere {
		return fmt.Errorf("%w: session %q is also indexed from another file",
			errNotAppendable, existing.ID)
	}

	res, err := parser.ParseAutoTail(jsonlPath, state.IndexedOffset, state.IndexedMsgCount)
	if err != nil {
		return err
	}
	if res.NewOffset <= state.IndexedOffset {
		// Only an unterminated trailing line so far and nothing to store — but the
		// pass has looked at the file, and that has to be recorded. Leaving the
		// stored size behind would keep NeedsReindex true, so every later scan of
		// every session with an unfinished last line re-reads the file for nothing.
		state.FileSize = info.Size()
		state.FileMtime = info.ModTime()
		state.IndexedAt = time.Now()
		if err := idx.store.UpsertIndexState(state); err != nil {
			return fmt.Errorf("update index state: %w", err)
		}
		return nil
	}

	// Mirrors the full path: daemon-internal extraction sessions are not indexed,
	// which also matters for one that only starts looking like one mid-file. Like
	// the full path this leaves the watermark untouched, so such a session stops
	// advancing and its later messages stay out of the index.
	if isExtractionPipelineSession(res.Messages) {
		return nil
	}

	// The watermark is only trustworthy while the store agrees with it: the rows
	// must be contiguous from sequence 0 and at least cover the watermark. A
	// mismatch means another writer, a binary that predates the watermark, an
	// interrupted pass or leftover duplicates — states in which appending could
	// leave a gap or a duplicate. The full path rebuilds the session instead.
	// More rows than the watermark covers are fine: that is a trailing line the
	// whole-file parse accepted but deliberately did not count, and the trim
	// below re-reads exactly that range from the file.
	count, minSeq, maxSeq, err := idx.store.SessionMessageStats(existing.ID)
	if err != nil {
		return err
	}
	contiguous := count == 0 || (minSeq == 0 && maxSeq == count-1)
	if !contiguous || count < state.IndexedMsgCount {
		return fmt.Errorf("%w: session %q holds %d messages (sequences %d..%d), watermark records %d",
			errNotAppendable, existing.ID, count, minSeq, maxSeq, state.IndexedMsgCount)
	}

	// Re-key the tail to the stored session: the tail parser cannot know a
	// subagent id (line.sessionId carries the parent) nor a Codex session id
	// (that header lives in the first line). The watermark identity wins.
	for i := range res.Messages {
		res.Messages[i].SessionID = existing.ID
		res.Messages[i].SourceAgent = existing.SourceAgent
	}

	// Metadata the first index could not see. Claude Code writes header lines
	// (file-history-snapshot, hooks, attachments) ahead of the first turn, so a
	// session indexed early carries an empty first_message, project and branch —
	// and the append is the only writer that ever reads the turn filling them.
	// UpsertSession heals instead of replacing, so a field that another pass
	// already set stays untouched.
	if existing.FirstMessage == "" && res.Meta.FirstUserMessage != "" {
		existing.FirstMessage = res.Meta.FirstUserMessage
		// A subagent's agent type is guessed from that first message, so a stored
		// guess was made from an empty string and has to be redone now.
		if existing.ParentSessionID != "" {
			existing.AgentType = guessAgentType(existing.FirstMessage)
		}
	}
	if existing.Project == "" && res.Meta.Project != "" {
		existing.Project = res.Meta.Project
		// The tail can be the first part of the file that names the project, and
		// the exclusion list is config: stop here if it turns out to be an excluded
		// one, the same way the full path does (it reads the project from its
		// parse). Like there, the watermark is left untouched, so such a session
		// stops advancing.
		short := models.ProjectShortFromPath(existing.Project)
		if idx.excludeProject[strings.ToLower(existing.Project)] || idx.excludeProject[short] {
			return nil
		}
	}
	if existing.GitBranch == "" && res.Meta.GitBranch != "" {
		existing.GitBranch = res.Meta.GitBranch
	}
	// The tail's first timestamp is not the session's start — the header lines
	// that carry it were indexed before this append — so it only fills a gap.
	if existing.StartedAt.IsZero() && !res.Meta.StartedAt.IsZero() {
		existing.StartedAt = res.Meta.StartedAt
	}

	existing.MessageCount = state.IndexedMsgCount + len(res.Messages)
	existing.JSONLSize = info.Size()
	existing.IndexedAt = time.Now()
	if !res.Meta.EndedAt.IsZero() && res.Meta.EndedAt.After(existing.EndedAt) {
		existing.EndedAt = res.Meta.EndedAt
	}
	if err := idx.store.UpsertSession(existing); err != nil {
		return fmt.Errorf("store session: %w", err)
	}

	// Trim anything a previous half-finished append left beyond the watermark.
	// Reads like belt and braces next to the count check above and is exactly
	// that: the check rules out the states where it matters, this keeps the
	// insert idempotent if such a state is ever reached anyway.
	//
	// Trim and insert are two statements, not one transaction. That is safe in the
	// direction that matters: the watermark is written last, so a crash between
	// them leaves rows that still match the watermark, and the next pass re-reads
	// the same range from the file — the full path (which is atomic) only runs when
	// the append refuses.
	if err := idx.store.DeleteMessagesFromSequence(existing.ID, state.IndexedMsgCount); err != nil {
		return err
	}
	if len(res.Messages) > 0 {
		if err := idx.store.InsertMessages(res.Messages); err != nil {
			return fmt.Errorf("store messages: %w", err)
		}
	}

	idx.storePulses(existing.ID, existing.ProjectShort, res.Messages)

	// AddTerms, not AddSession: the pre-append terms must survive.
	idx.bloom.AddTerms(existing.ID, extractTerms(res.Messages))

	assocs := buildAssociations(existing.ID, existing.ProjectShort, res.Messages)
	if len(assocs) > 0 {
		if err := idx.store.InsertAssociationBatch(assocs); err != nil {
			log.Printf("warn: associations for %s: %v", existing.ID, err)
		}
	}

	idx.updateCoverage(existing.ProjectShort, res.Messages)

	if idx.archiver != nil {
		if err := idx.archiver.ArchiveFile(jsonlPath, projectDirName(jsonlPath)); err != nil {
			log.Printf("warn: archive %s: %v", jsonlPath, err)
		}
	}

	if err := idx.store.UpsertIndexState(&models.IndexState{
		JSONLPath:         jsonlPath,
		FileSize:          info.Size(),
		FileMtime:         info.ModTime(),
		IndexedAt:         time.Now(),
		IndexedOffset:     res.NewOffset,
		IndexedSessionID:  existing.ID,
		IndexedMsgCount:   existing.MessageCount,
		IndexedPrefixHash: res.PrefixHash,
	}); err != nil {
		return fmt.Errorf("update index state: %w", err)
	}

	return nil
}

// indexFullSession re-parses the whole file and replaces everything stored for
// the session.
func (idx *Indexer) indexFullSession(jsonlPath string, info os.FileInfo) error {
	// 2. Parse JSONL
	res, err := parser.ParseAutoTail(jsonlPath, 0, 0)
	if err != nil {
		return fmt.Errorf("parse %s: %w", jsonlPath, err)
	}
	messages, meta := res.Messages, res.Meta
	if meta.SessionID == "" {
		return nil // empty or unparseable session
	}

	// Skip daemon-internal extraction sessions (cluster labeling, evolution, etc.)
	// to prevent self-referential backlog. Uses the same prompt signatures as
	// the OpencodeScanner filter.
	if isExtractionPipelineSession(messages) {
		return nil
	}
	meta.SourceAgent = models.NormalizeSourceAgent(meta.SourceAgent)
	meta.SessionID = models.NormalizeSessionID(meta.SourceAgent, meta.SessionID)
	for i := range messages {
		messages[i].SessionID = models.NormalizeSessionID(meta.SourceAgent, messages[i].SessionID)
		messages[i].SourceAgent = meta.SourceAgent
	}

	// 3. Build session record
	projectShort := models.ProjectShortFromPath(meta.Project)

	// Skip projects on the exclusion list
	if idx.excludeProject[strings.ToLower(meta.Project)] || idx.excludeProject[projectShort] {
		return nil
	}
	sess := &models.Session{
		ID:           meta.SessionID,
		Project:      meta.Project,
		ProjectShort: projectShort,
		GitBranch:    meta.GitBranch,
		FirstMessage: meta.FirstUserMessage,
		MessageCount: len(messages),
		StartedAt:    meta.StartedAt,
		EndedAt:      meta.EndedAt,
		JSONLPath:    jsonlPath,
		JSONLSize:    info.Size(),
		IndexedAt:    time.Now(),
		SourceAgent:  meta.SourceAgent,
	}

	// For subagent sessions: use agentId as session ID, sessionId as parent
	if meta.AgentID != "" {
		sess.ID = models.NormalizeSessionID(meta.SourceAgent, meta.AgentID)
		sess.ParentSessionID = meta.SessionID
		sess.AgentType = guessAgentType(meta.FirstUserMessage)
	}

	// Serialize per session id, not per path: two files can carry the same id (a
	// resumed session keeps the parent id behind a snapshot header, a restored copy
	// duplicates it), and both would otherwise replace the same session's rows at
	// the same time. Taken after the parse — that is the expensive part — and in
	// the same order as the append path (path lock, then session lock), so the two
	// cannot deadlock.
	unlockSession := idx.sessionLocks.lock(sess.ID)
	defer unlockSession()

	// 4. Store session + messages in SQLite
	if err := idx.store.UpsertSession(sess); err != nil {
		return fmt.Errorf("store session: %w", err)
	}
	// Every row carries the session's own id. The store keys its deletion on that
	// id, so a file whose turn lines name a different id — the first line with an
	// id wins for the session (parser.go) — would insert rows under the other id,
	// collide with them on the next pass and never advance its watermark again.
	// The append path re-keys its tail the same way.
	for i := range messages {
		messages[i].SessionID = sess.ID
	}
	if err := idx.store.ReplaceMessagesBySession(sess.ID, messages); err != nil {
		return fmt.Errorf("store messages: %w", err)
	}

	// 4b. Store pulse learnings from away_summary events
	idx.storePulses(sess.ID, projectShort, messages)

	// 5. FTS5 indexing happens automatically via InsertMessages() — no separate step needed

	// 6. Update bloom filter (use sess.ID, not meta.SessionID — for subagents these differ)
	terms := extractTerms(messages)
	idx.bloom.AddSession(sess.ID, terms)

	// 7. Build associations
	assocs := buildAssociations(sess.ID, projectShort, messages)
	if len(assocs) > 0 {
		if err := idx.store.InsertAssociationBatch(assocs); err != nil {
			log.Printf("warn: associations for %s: %v", sess.ID, err)
		}
	}

	// 8. Update file coverage
	idx.updateCoverage(projectShort, messages)

	// 9. Archive JSONL
	if idx.archiver != nil {
		project := projectDirName(jsonlPath)
		if err := idx.archiver.ArchiveFile(jsonlPath, project); err != nil {
			log.Printf("warn: archive %s: %v", jsonlPath, err)
		}
	}

	// 10. Update index state — the watermark records where parsing stopped so the
	// next run can append instead of re-reading the file. res.NewOffset (not the
	// file size) is the anchor: a trailing line that was still being written is
	// re-read from its own start rather than from halfway through it, and the
	// message count is the one belonging to that offset, not the total. The
	// fingerprint comes from the parse, so a rewrite during this pass cannot make
	// it describe a file state that was never indexed.
	if err := idx.store.UpsertIndexState(&models.IndexState{
		JSONLPath:         jsonlPath,
		FileSize:          info.Size(),
		FileMtime:         info.ModTime(),
		IndexedAt:         time.Now(),
		IndexedOffset:     res.NewOffset,
		IndexedSessionID:  sess.ID,
		IndexedMsgCount:   res.CompleteCount,
		IndexedPrefixHash: res.PrefixHash,
	}); err != nil {
		return fmt.Errorf("update index state: %w", err)
	}

	return nil
}

// storePulses persists away_summary recaps as pulse learnings. At most one pulse
// per session is kept, so re-running over the same messages is harmless.
func (idx *Indexer) storePulses(sessionID, projectShort string, msgs []models.Message) {
	for _, msg := range msgs {
		if msg.MessageType != "pulse" {
			continue
		}
		exists, err := idx.store.HasPulseForSession(sessionID)
		if err != nil {
			log.Printf("warn: pulse dedup check for %s: %v", sessionID, err)
			continue
		}
		if exists {
			continue
		}
		_, err = idx.store.InsertLearning(&models.Learning{
			SessionID:  sessionID,
			Category:   "pulse",
			Content:    msg.Content,
			Project:    projectShort,
			Confidence: 1.0,
			Source:     "system_captured",
			ModelUsed:  "cc_recap",
			CreatedAt:  msg.Timestamp,
		})
		if err != nil {
			log.Printf("warn: pulse learning for %s: %v", sessionID, err)
		}
	}
}

// ProgressFunc is called during IndexAll to report progress.
// total=-1 means total is not yet known.
type ProgressFunc func(indexed, skipped, total int)

// IndexAll scans a Claude Code projects directory and indexes all sessions.
func (idx *Indexer) IndexAll(projectsDir string) (indexed, skipped int, err error) {
	return idx.IndexAllWithProgress(projectsDir, nil)
}

// IndexAllWithProgress scans and indexes all sessions, calling progressFn after each file.
func (idx *Indexer) IndexAllWithProgress(projectsDir string, progressFn ProgressFunc) (indexed, skipped int, err error) {
	jsonlFiles, err := idx.collectSessionFiles(projectsDir)
	if err != nil {
		return 0, 0, err
	}

	total := len(jsonlFiles)
	for _, jsonlPath := range jsonlFiles {
		did, indexErr := idx.tryIndexFile(jsonlPath)
		if indexErr != nil {
			log.Printf("warn: index %s: %v", jsonlPath, indexErr)
		}
		if did {
			indexed++
		} else {
			skipped++
		}
		if progressFn != nil {
			progressFn(total, indexed+skipped, skipped)
		}
	}

	return indexed, skipped, nil
}

func (idx *Indexer) collectSessionFiles(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat sessions dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sessions path is not a directory: %s", root)
	}

	var jsonlFiles []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "archive" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		jsonlFiles = append(jsonlFiles, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan sessions dir: %w", err)
	}
	sort.Strings(jsonlFiles)
	return jsonlFiles, nil
}

func extractTerms(msgs []models.Message) []string {
	var terms []string
	for _, m := range msgs {
		if m.Content != "" {
			terms = append(terms, m.Content)
		}
		if m.ToolName != "" {
			terms = append(terms, m.ToolName)
		}
		if m.FilePath != "" {
			terms = append(terms, m.FilePath)
		}
	}
	return terms
}

func buildAssociations(sessionID, project string, msgs []models.Message) []models.Association {
	var assocs []models.Association

	// session → project
	if project != "" {
		assocs = append(assocs, models.Association{
			SourceType: "session", SourceID: sessionID,
			TargetType: "project", TargetID: project, Weight: 1,
		})
	}

	seen := map[string]bool{}
	for _, m := range msgs {
		// session → file
		if m.FilePath != "" {
			key := "file:" + m.FilePath
			if !seen[key] {
				assocs = append(assocs, models.Association{
					SourceType: "session", SourceID: sessionID,
					TargetType: "file", TargetID: m.FilePath, Weight: 1,
				})
				seen[key] = true
			}
		}
		// session → command (normalized: first word of Bash commands)
		if m.ToolName == "Bash" && m.Content != "" {
			cmd := normalizeCommand(m.Content)
			key := "command:" + cmd
			if !seen[key] && cmd != "" {
				assocs = append(assocs, models.Association{
					SourceType: "session", SourceID: sessionID,
					TargetType: "command", TargetID: cmd, Weight: 1,
				})
				seen[key] = true
			}
		}
	}
	return assocs
}

func normalizeCommand(cmd string) string {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return ""
	}
	// Return first word (e.g. "docker" from "docker-compose up -d")
	return strings.ToLower(parts[0])
}

func (idx *Indexer) updateCoverage(project string, msgs []models.Message) {
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.FilePath == "" || seen[m.FilePath] {
			continue
		}
		seen[m.FilePath] = true

		op := "read"
		if m.ToolName == "Edit" || m.ToolName == "Write" {
			op = "write"
		} else if m.ToolName == "Grep" {
			op = "grep"
		} else if m.ToolName == "Bash" {
			op = "bash"
		}

		idx.store.UpsertFileCoverage(&models.FileCoverage{
			Project:        project,
			FilePath:       m.FilePath,
			Directory:      filepath.Dir(m.FilePath),
			SessionCount:   1,
			LastTouched:    time.Now().Format(time.RFC3339),
			OperationTypes: op,
		})
	}
}

func projectDirName(jsonlPath string) string {
	if strings.Contains(filepath.Clean(jsonlPath), string(os.PathSeparator)+".codex"+string(os.PathSeparator)+"sessions"+string(os.PathSeparator)) {
		return "codex"
	}
	// Regular:  .../projects/-var-www-html-ccm19/session.jsonl → -var-www-html-ccm19
	// Subagent: .../projects/-var-www-html-ccm19/<session>/subagents/agent.jsonl → -var-www-html-ccm19
	dir := filepath.Dir(jsonlPath)
	if filepath.Base(dir) == "subagents" {
		// Walk up past subagents/ and session-id/
		return filepath.Base(filepath.Dir(filepath.Dir(dir)))
	}
	return filepath.Base(dir)
}

// tryIndexFile checks whether a JSONL file needs reindexing and, if so,
// runs the full indexing pipeline. It returns (true, nil) when the file was
// indexed, (false, nil) when it was skipped (already current or unreadable),
// and (false, err) when IndexSession failed.
func (idx *Indexer) tryIndexFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, nil // unreadable — caller counts as skipped
	}
	if !idx.store.NeedsReindex(path, info.Size(), info.ModTime()) {
		return false, nil // already up to date
	}
	if err := idx.IndexSession(path); err != nil {
		return false, err
	}
	return true, nil
}

// guessAgentType infers the agent type from the first user message.
// Uses kebab-case consistently, matching Claude Code's subagent_type values.
func guessAgentType(firstMessage string) string {
	lower := strings.ToLower(firstMessage)
	switch {
	case strings.Contains(lower, "explore") || strings.Contains(lower, "find files") || strings.Contains(lower, "search code"):
		return "explore"
	case strings.Contains(lower, "write a plan") || strings.Contains(lower, "design a plan") || strings.Contains(lower, "implementation plan"):
		return "plan"
	case strings.Contains(lower, "review") || strings.Contains(lower, "code-review"):
		return "code-reviewer"
	default:
		return "general-purpose"
	}
}
