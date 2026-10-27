package indexer

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/archive"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"

	_ "modernc.org/sqlite"
)

const scanOpencodeInterval = 5 * time.Minute

type OpencodeScanner struct {
	dbPath         string
	sourceKey      string
	store          *storage.Store
	archiver       *archive.Archiver
	scanMu         sync.Mutex
	lastScanAt     time.Time
	lastSessionID  string
	lastCheckAt    time.Time
	logger         *log.Logger
	notifyCh       chan<- struct{}
	excludeProject map[string]bool
}

func NewOpencodeScanner(dbPath string, store *storage.Store, logger *log.Logger, notifyCh chan<- struct{}, excludeProjects []string, arch *archive.Archiver) *OpencodeScanner {
	exclude := normalizeExcludedProjects(excludeProjects)
	sourceKey, err := filepath.Abs(dbPath)
	if err != nil {
		sourceKey = filepath.Clean(dbPath)
	}
	scanner := &OpencodeScanner{
		dbPath:         dbPath,
		sourceKey:      sourceKey,
		store:          store,
		archiver:       arch,
		lastCheckAt:    time.Now(),
		logger:         logger,
		notifyCh:       notifyCh,
		excludeProject: exclude,
	}
	state, found, err := store.GetOpencodeScanState(sourceKey)
	if err != nil {
		scanner.logf("opencode scanner: load watermark: %v", err)
	} else if found {
		scanner.lastScanAt = time.UnixMilli(state.LastUpdatedMs)
		scanner.lastSessionID = state.LastSessionID
	}
	return scanner
}

func (s *OpencodeScanner) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

// sessionExportFormat marks the document written for a session that has no JSONL
// file of its own, so a later reader can tell layouts apart.
const sessionExportFormat = "yesmem-session-export/1"

// sessionExport is one session's archived record: the indexed row plus its
// messages, which is everything yesmem knows about the session.
type sessionExport struct {
	Format   string           `json:"format"`
	Session  *models.Session  `json:"session"`
	Messages []models.Message `json:"messages"`
}

// writeSessionExport writes the session into the archive. Opencode sessions live
// in opencode.db, a database the archive exists to outlive, so they are exported
// as one document per session instead of being copied file by file. The encoder
// writes straight to the file: a long session would otherwise be materialised as
// one buffer before anything reaches the disk.
//
// It is a package function so the catch-up path (ArchiveMissingSessions) writes
// byte-identical documents; a session that arrives through either path must not
// be recognisable by its shape.
func writeSessionExport(arch *archive.Archiver, sess *models.Session, msgs []models.Message) error {
	return arch.WriteSessionDoc(
		archive.ProjectDirName(sess.Project),
		archiveDocumentName(sess.ID),
		func(w io.Writer) error {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			return enc.Encode(sessionExport{
				Format:   sessionExportFormat,
				Session:  sess,
				Messages: msgs,
			})
		},
	)
}

// archiveDocumentName turns a session id into a file name. Normalized ids carry
// the source agent as a prefix ("opencode:ses_..."); the colon is legal on disk
// but not portable, and separators would escape the project folder.
func archiveDocumentName(sessionID string) string {
	name := strings.NewReplacer(":", "-", "/", "-", "\\", "-").Replace(sessionID)
	switch name {
	case "", ".", "..":
		return "session.json"
	}
	return name + ".json"
}

// openOpencodeDB opens the opencode session database. The file belongs to another
// application: the scanner only reads it (querySessions, queryMessages), so the
// handle is opened read-only and refuses writes. SQLite still keeps its own WAL
// bookkeeping files beside a WAL-mode database; that is the driver's business, the
// point here is that no statement of ours can modify the foreign database. It is a
// function of its own so the handle the scanner works with can be tested directly.
func openOpencodeDB(path string) (*sql.DB, error) {
	return storage.OpenSQLiteReadOnly(path)
}

func (s *OpencodeScanner) MaybeScan() int {
	// MaybeScan is called both by the daemon ticker and idle ticks. Do not make
	// either caller wait behind a potentially long full scan.
	if !s.scanMu.TryLock() {
		return 0
	}
	defer s.scanMu.Unlock()

	if time.Since(s.lastCheckAt) < scanOpencodeInterval {
		return 0
	}
	// Rate-limit from completion, not start. Otherwise a scan taking longer
	// than the interval can be followed immediately by another scan.
	defer func() { s.lastCheckAt = time.Now() }()

	db, err := openOpencodeDB(s.dbPath)
	if err != nil {
		s.logf("opencode scanner: open db: %v", err)
		return 0
	}
	defer db.Close()

	db.SetMaxOpenConns(1)

	sessions, err := s.querySessions(db)
	if err != nil {
		s.logf("opencode scanner: query sessions: %v", err)
		return 0
	}

	if len(sessions) == 0 {
		return 0
	}

	var newlyIndexed int
	watermarkAt := s.lastScanAt
	watermarkSessionID := s.lastSessionID
	watermarkBlocked := false
	advanceWatermark := func(sess opencodeDBSession) {
		if watermarkBlocked {
			return
		}
		watermarkAt = sess.Updated
		watermarkSessionID = sess.ID
	}
	for _, sess := range sessions {
		dbMsgs, err := s.queryMessages(db, sess.ID)
		if err != nil {
			s.logf("opencode scanner: session %q messages: %v", sess.ID, err)
			watermarkBlocked = true
			continue
		}

		msgs := mapOpencodeMessages(dbMsgs)
		for i := range msgs {
			msgs[i].Model = sess.Model
		}
		if isExtractionPipelineSession(msgs) {
			advanceWatermark(sess)
			continue
		}
		yesSess := buildOpencodeSession(sess, msgs)

		// Skip projects on the exclusion list
		if projectExcluded(s.excludeProject, yesSess.Project, yesSess.ProjectShort) {
			advanceWatermark(sess)
			continue
		}

		if err := s.store.UpsertSession(yesSess); err != nil {
			s.logf("opencode scanner: upsert session %q: %v", yesSess.ID, err)
			watermarkBlocked = true
			continue
		}

		s.normalizeMessages(msgs, yesSess.ID)

		if err := s.store.DeleteMessagesBySession(yesSess.ID); err != nil {
			s.logf("opencode scanner: delete old messages %q: %v", yesSess.ID, err)
			watermarkBlocked = true
			continue
		}
		if len(msgs) > 0 {
			if err := s.store.InsertMessages(msgs); err != nil {
				s.logf("opencode scanner: insert messages %q: %v", yesSess.ID, err)
				watermarkBlocked = true
				continue
			}
		}

		newlyIndexed++
		advanceWatermark(sess)
		if s.archiver != nil {
			if err := writeSessionExport(s.archiver, yesSess, msgs); err != nil {
				// The archive is a second copy, not the index: a failure there must
				// not keep the session out of the search path.
				s.logf("opencode scanner: archive session %q: %v", yesSess.ID, err)
			}
		}
	}

	if !watermarkAt.Equal(s.lastScanAt) || watermarkSessionID != s.lastSessionID {
		state := storage.OpencodeScanState{
			LastUpdatedMs: watermarkAt.UnixMilli(),
			LastSessionID: watermarkSessionID,
		}
		if err := s.store.SetOpencodeScanState(s.sourceKey, state); err != nil {
			s.logf("opencode scanner: persist watermark: %v", err)
		} else {
			s.lastScanAt = watermarkAt
			s.lastSessionID = watermarkSessionID
		}
	}

	if newlyIndexed > 0 {
		s.logf("opencode scanner: indexed %d sessions", newlyIndexed)
		if s.notifyCh != nil {
			select {
			case s.notifyCh <- struct{}{}:
			default:
			}
		}
	}
	return newlyIndexed
}

func (s *OpencodeScanner) normalizeMessages(msgs []models.Message, sessionID string) {
	for i := range msgs {
		msgs[i].SessionID = sessionID
		msgs[i].SourceAgent = models.SourceAgentOpencode
	}
}

func (s *OpencodeScanner) querySessions(db *sql.DB) ([]opencodeDBSession, error) {
	var rows *sql.Rows
	var err error

	if s.lastScanAt.IsZero() {
		rows, err = db.Query(`
			SELECT id, directory, title,
				time_created, time_updated, parent_id,
				COALESCE(model, ''), COALESCE(agent, '')
			FROM session
			ORDER BY time_updated, id
		`)
	} else {
		rows, err = db.Query(`
			SELECT id, directory, title,
				time_created, time_updated, parent_id,
				COALESCE(model, ''), COALESCE(agent, '')
			FROM session
			WHERE time_updated > ? OR (time_updated = ? AND id > ?)
			ORDER BY time_updated, id
		`, s.lastScanAt.UnixMilli(), s.lastScanAt.UnixMilli(), s.lastSessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var sessions []opencodeDBSession
	for rows.Next() {
		var sess opencodeDBSession
		var createdMs, updatedMs int64
		var parentID sql.NullString
		var model, agent string
		if err := rows.Scan(&sess.ID, &sess.Directory, &sess.Title,
			&createdMs, &updatedMs, &parentID,
			&model, &agent); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		if parentID.Valid {
			sess.ParentID = parentID.String
		}
		sess.Created = time.UnixMilli(createdMs)
		sess.Updated = time.UnixMilli(updatedMs)
		sess.Model = model
		sess.Agent = agent
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

func (s *OpencodeScanner) queryMessages(db *sql.DB, sessionID string) ([]opencodeDBMessage, error) {
	rows, err := db.Query(`
		SELECT m.id, m.session_id,
			json_extract(m.data, '$.role') as role,
			m.time_created,
			p.data as part_data
		FROM message m
		JOIN part p ON p.message_id = m.id
		WHERE m.session_id = ?
		ORDER BY m.time_created, m.id, p.id
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var msgs []opencodeDBMessage
	for rows.Next() {
		var msg opencodeDBMessage
		var createdMs int64
		var partJSON string
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Role, &createdMs, &partJSON); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msg.CreatedMs = createdMs
		if err := json.Unmarshal([]byte(partJSON), &msg.Part); err != nil {
			return nil, fmt.Errorf("parse part: %w", err)
		}
		msgs = append(msgs, msg)
	}
	sort.Slice(msgs, func(i, j int) bool {
		if msgs[i].CreatedMs != msgs[j].CreatedMs {
			return msgs[i].CreatedMs < msgs[j].CreatedMs
		}
		return msgs[i].ID < msgs[j].ID
	})
	return msgs, rows.Err()
}

var extractionPromptSignatures = storage.ExtractionSessionSignatures

func isExtractionPipelineSession(msgs []models.Message) bool {
	for _, m := range msgs {
		if m.Role == "user" {
			for _, sig := range extractionPromptSignatures {
				if strings.Contains(m.Content, sig) {
					return true
				}
			}
		}
	}
	return false
}
