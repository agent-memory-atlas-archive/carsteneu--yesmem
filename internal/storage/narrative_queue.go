package storage

import (
	"fmt"
	"strings"
)

// narrativeIDBatch bounds the placeholder count of an IN list whose length comes
// from the database rather than from a constant.
const narrativeIDBatch = 500

// notQuarantinedSessionWhere excludes the sessions whose learnings a quarantine
// marked. Crash recovery quarantines the learnings of a failed session, and a
// fresh narrative would carry that content back into recall through the narrative
// path, which carries no quarantine of its own. Excluding such a session here also
// keeps it out of the coverage total: it can never become a narrative, so counting
// it as missing would report a gap no run can close.
//
// The gate sees quarantined learning rows, and that is all there is to see: the
// quarantine is a per-row flag (QuarantineSession updates existing rows), so a
// session quarantined before its first extraction leaves no trace and stays
// invisible here.
const notQuarantinedSessionWhere = `NOT EXISTS (
			SELECT 1 FROM learnings q
			WHERE q.session_id = s.id AND COALESCE(q.quarantined_at, '') <> ''
		)`

// PendingNarrativeSessionIDs returns the sessions that still owe a narrative and
// that the generator can actually summarize.
//
// Two databases are involved: sessions and learnings live in the main DB, the
// message bodies in messages.db, so the text-message gate cannot be a join. It is
// evaluated here in Go, against one bulk count per batch of candidates.
//
// Why the gate matters: the generator builds its input from text messages that
// carry content and skips a session that has fewer than minTexts of them. A
// pending count that only reads sessions.message_count therefore reports work the
// generator can never do, and every cycle reloads those sessions without a result.
func (s *Store) PendingNarrativeSessionIDs(minTexts int) (map[string]struct{}, error) {
	if minTexts < 1 {
		minTexts = 1
	}
	candidates, err := s.sessionIDsWhere(`s.narrative_at IS NULL
		AND s.message_count >= ?
		AND `+notQuarantinedSessionWhere, minTexts)
	if err != nil {
		return nil, err
	}
	return s.filterByTextMessages(candidates, minTexts)
}

// SessionQuarantined reports whether any learning of the session carries the
// quarantine mark. The generators check it directly, so every entry point honours
// the gate and not only the batch path that reads the pending set.
func (s *Store) SessionQuarantined(sessionID string) (bool, error) {
	var n int
	err := s.readerDB().QueryRow(`SELECT COUNT(*) FROM learnings
		WHERE session_id = ? AND COALESCE(quarantined_at, '') <> ''`, sessionID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("session quarantine: %w", err)
	}
	return n > 0, nil
}

// sessionIDsWhere lists the session ids that match a predicate over the alias s.
func (s *Store) sessionIDsWhere(where string, args ...any) ([]string, error) {
	rows, err := s.readerDB().Query(`SELECT s.id FROM sessions s WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list session ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list session ids: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list session ids: %w", err)
	}
	return ids, nil
}

// filterByTextMessages keeps the ids whose session carries at least minTexts
// text messages with content, which is the input the narrative generator needs.
func (s *Store) filterByTextMessages(ids []string, minTexts int) (map[string]struct{}, error) {
	counts, err := s.textMessageCounts(ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if counts[id] >= minTexts {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// textMessageCounts counts, per session id, the messages a narrative can use.
// The id list comes from the caller's database, so it is batched: one placeholder
// per id would run into SQLite's variable limit on a large installation.
func (s *Store) textMessageCounts(ids []string) (map[string]int, error) {
	counts := make(map[string]int, len(ids))
	for _, batch := range chunkStrings(ids, narrativeIDBatch) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.messagesReaderDB().Query(`SELECT session_id, COUNT(*)
			FROM messages
			WHERE message_type = 'text' AND COALESCE(content, '') <> ''
			  AND session_id IN (`+placeholders+`)
			GROUP BY session_id`, args...)
		if err != nil {
			return nil, fmt.Errorf("count text messages: %w", err)
		}
		for rows.Next() {
			var id string
			var count int
			if err := rows.Scan(&id, &count); err != nil {
				rows.Close()
				return nil, fmt.Errorf("count text messages: scan: %w", err)
			}
			counts[id] = count
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("count text messages: %w", err)
		}
		rows.Close()
	}
	return counts, nil
}

// activeNarrativeSessionIDs returns the subset of ids whose session carries a
// narrative row that recall would actually return.
func (s *Store) activeNarrativeSessionIDs(ids []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(ids))
	for _, batch := range chunkStrings(ids, narrativeIDBatch) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.readerDB().Query(`SELECT DISTINCT session_id FROM learnings
			WHERE category = 'narrative' AND superseded_by IS NULL
			  AND COALESCE(quarantined_at, '') = ''
			  AND session_id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("list active narratives: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("list active narratives: scan: %w", err)
			}
			out[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list active narratives: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// sessionIDsWithNarrativeMarker returns the subset of ids whose session carries
// the narrative_at marker, whether or not the row still exists.
func (s *Store) sessionIDsWithNarrativeMarker(ids []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(ids))
	for _, batch := range chunkStrings(ids, narrativeIDBatch) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.readerDB().Query(`SELECT id FROM sessions
			WHERE narrative_at IS NOT NULL AND id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("list narrative markers: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("list narrative markers: scan: %w", err)
			}
			out[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list narrative markers: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// chunkStrings splits ids into batches of at most size entries.
func chunkStrings(ids []string, size int) [][]string {
	if size < 1 || len(ids) == 0 {
		return nil
	}
	var batches [][]string
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		batches = append(batches, ids[start:end])
	}
	return batches
}
