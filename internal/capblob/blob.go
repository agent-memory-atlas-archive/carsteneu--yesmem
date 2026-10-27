package capblob

import (
	"bufio"
	"io"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/storage"
)

type Store interface {
	CapStoreCreateTable(capName, tableName string, columns []storage.ColumnDef) error
	CapStoreUpsert(capName, tableName string, data map[string]any) (int64, error)
	CapStoreQuery(capName, tableName, where string, args []any, limit int) ([]map[string]any, error)
	CapStoreDelete(capName, tableName, where string, args []any) (int64, error)
}

const (
	DefaultChunkSize = 20000
	TableName        = "blobs"
	QueryLimit       = 1000
)

func schema() []storage.ColumnDef {
	return []storage.ColumnDef{
		{Name: "key", Type: "TEXT"},
		{Name: "chunk_idx", Type: "INTEGER"},
		{Name: "data", Type: "TEXT"},
	}
}

// Put stores r as a chunked blob. Chunks end on a rune boundary: a byte-wise
// cut can land inside a multi-byte rune, and the chunk crosses a JSON boundary
// on its way to storage (the daemon RPC marshals the cell), where an invalid
// tail is rewritten to U+FFFD — the blob would come back corrupted.
//
// A chunk is at most chunkSize bytes, extended by up to utf8.UTFMax-1 when the
// rune at the limit spans it. That ceiling is a hard bound even for input that
// is not valid UTF-8 and so never reaches a boundary: without it the chunk grows
// to the whole payload, which the store refuses past its cell limit and which is
// held in memory in full until then. Only the last chunk may be shorter.
func Put(s Store, capName, key string, r io.Reader, chunkSize int) error {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	if err := s.CapStoreCreateTable(capName, TableName, schema()); err != nil {
		return err
	}
	if _, err := s.CapStoreDelete(capName, TableName, "key=?", []any{key}); err != nil {
		return err
	}

	br := bufio.NewReader(r)
	chunk := make([]byte, 0, chunkSize+utf8.UTFMax)
	idx := 0
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if _, err := s.CapStoreUpsert(capName, TableName, map[string]any{
			"key":       key,
			"chunk_idx": idx,
			"data":      string(chunk),
		}); err != nil {
			return err
		}
		idx++
		chunk = chunk[:0]
		return nil
	}

	hardLimit := chunkSize + utf8.UTFMax - 1
	for {
		b, err := br.ReadByte()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		chunk = append(chunk, b)
		if len(chunk) >= chunkSize && (endsAtRuneBoundary(chunk) || len(chunk) >= hardLimit) {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// endsAtRuneBoundary reports whether b holds whole runes. DecodeLastRune
// reports an incomplete trailing sequence as (RuneError, 1); a literal U+FFFD
// in the input arrives as three bytes and is a boundary like any other rune.
func endsAtRuneBoundary(b []byte) bool {
	r, size := utf8.DecodeLastRune(b)
	return !(r == utf8.RuneError && size <= 1)
}

// Get reassembles the blob stored under key. CapsQuery returns at most
// QueryLimit rows, so a blob that spans more chunks is read page by page —
// a single query silently dropped everything past the first page.
func Get(s Store, capName, key string, w io.Writer) error {
	byIdx := make(map[int]string)
	maxIdx := -1

	// CapsQuery always orders by id DESC, so paging walks downwards: each page
	// holds the next QueryLimit rows below the lowest id seen so far.
	beforeID := 0
	for {
		where := "key=?"
		args := []any{key}
		if beforeID > 0 {
			where += " AND id < ?"
			args = append(args, beforeID)
		}
		rows, err := s.CapStoreQuery(capName, TableName, where, args, QueryLimit)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}

		pageMinID := 0
		for _, row := range rows {
			if idx, ok := rowInt(row["chunk_idx"]); ok {
				if data, dok := row["data"].(string); dok {
					byIdx[idx] = data
					if idx > maxIdx {
						maxIdx = idx
					}
				}
			}
			if id, ok := rowInt(row["id"]); ok && (pageMinID == 0 || id < pageMinID) {
				pageMinID = id
			}
		}
		// The cursor has to move, or a store that ignores the filter would spin.
		if len(rows) < QueryLimit || pageMinID == 0 || pageMinID <= beforeID {
			break
		}
		beforeID = pageMinID
	}

	for i := 0; i <= maxIdx; i++ {
		chunk, ok := byIdx[i]
		if !ok {
			continue
		}
		if _, werr := w.Write([]byte(chunk)); werr != nil {
			return werr
		}
	}
	return nil
}

// rowInt reads an integer cell. The RPC path decodes JSON, where every number
// arrives as a float64; a direct store call yields an int64.
func rowInt(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	case float64:
		return int(n), true
	}
	return 0, false
}
