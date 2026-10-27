package capblob

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/storage"
)

type mockRow struct {
	id   int64
	data map[string]any
}

type mockStore struct {
	tables map[string]map[string]bool
	rows   map[string][]mockRow
	nextID int64
}

func newMockStore() *mockStore {
	return &mockStore{
		tables: map[string]map[string]bool{},
		rows:   map[string][]mockRow{},
		nextID: 1,
	}
}

func (m *mockStore) tableKey(cap, tbl string) string {
	return cap + "::" + tbl
}

func (m *mockStore) CapStoreCreateTable(capName, tableName string, columns []storage.ColumnDef) error {
	if m.tables[capName] == nil {
		m.tables[capName] = map[string]bool{}
	}
	m.tables[capName][tableName] = true
	return nil
}

func (m *mockStore) CapStoreUpsert(capName, tableName string, data map[string]any) (int64, error) {
	if !m.tables[capName][tableName] {
		return 0, fmt.Errorf("table not created")
	}
	k := m.tableKey(capName, tableName)
	id := m.nextID
	m.nextID++
	row := mockRow{id: id, data: map[string]any{"id": id}}
	for kk, vv := range data {
		row.data[kk] = vv
	}
	m.rows[k] = append(m.rows[k], row)
	return id, nil
}

func (m *mockStore) CapStoreQuery(capName, tableName, where string, args []any, limit int) ([]map[string]any, error) {
	k := m.tableKey(capName, tableName)
	rows := filterMockRows(m.rows[k], where, args)

	// CapsQuery orders by id DESC — the newest chunk comes first, and a limit
	// therefore cuts off the OLDEST rows, not the newest.
	sort.Slice(rows, func(i, j int) bool { return rows[i].id > rows[j].id })

	var out []map[string]any
	for _, row := range rows {
		copyRow := map[string]any{}
		for kk, vv := range row.data {
			copyRow[kk] = vv
		}
		if idx, ok := copyRow["chunk_idx"].(int); ok {
			copyRow["chunk_idx"] = int64(idx)
		}
		out = append(out, copyRow)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func filterMockRows(rows []mockRow, where string, args []any) []mockRow {
	switch {
	case where == "":
		return rows
	case where == "key=?" && len(args) == 1:
		var out []mockRow
		for _, row := range rows {
			if row.data["key"] == args[0] {
				out = append(out, row)
			}
		}
		return out
	case where == "key=? AND id < ?" && len(args) == 2:
		before, ok := mockInt(args[1])
		if !ok {
			return nil
		}
		var out []mockRow
		for _, row := range rows {
			if row.data["key"] == args[0] && row.id < before {
				out = append(out, row)
			}
		}
		return out
	}
	return rows
}

func mockInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func (m *mockStore) CapStoreDelete(capName, tableName, where string, args []any) (int64, error) {
	k := m.tableKey(capName, tableName)
	var kept []mockRow
	var deleted int64
	for _, row := range m.rows[k] {
		if where == "key=?" && len(args) == 1 && row.data["key"] == args[0] {
			deleted++
			continue
		}
		kept = append(kept, row)
	}
	m.rows[k] = kept
	return deleted, nil
}

func TestPutGet_SingleChunk(t *testing.T) {
	s := newMockStore()
	want := "hello world"
	if err := Put(s, "testcap", "k1", strings.NewReader(want), DefaultChunkSize); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var buf bytes.Buffer
	if err := Get(s, "testcap", "k1", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != want {
		t.Fatalf("mismatch: got %q want %q", buf.String(), want)
	}
}

func TestPutGet_MultiChunk(t *testing.T) {
	s := newMockStore()
	payload := strings.Repeat("abcdefgh", 40000)
	if len(payload) < DefaultChunkSize*3 {
		t.Fatalf("test payload too small: %d", len(payload))
	}
	if err := Put(s, "testcap", "big", strings.NewReader(payload), DefaultChunkSize); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rows, _ := s.CapStoreQuery("testcap", TableName, "key=?", []any{"big"}, QueryLimit)
	if len(rows) < 3 {
		t.Fatalf("expected >=3 chunks, got %d", len(rows))
	}
	var buf bytes.Buffer
	if err := Get(s, "testcap", "big", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != payload {
		t.Fatalf("size mismatch: got %d want %d", buf.Len(), len(payload))
	}
}

func TestPut_OverwriteShrinks(t *testing.T) {
	s := newMockStore()
	big := strings.Repeat("X", DefaultChunkSize*3+100)
	if err := Put(s, "c", "k", strings.NewReader(big), DefaultChunkSize); err != nil {
		t.Fatalf("Put big: %v", err)
	}
	small := "tiny"
	if err := Put(s, "c", "k", strings.NewReader(small), DefaultChunkSize); err != nil {
		t.Fatalf("Put small: %v", err)
	}
	rows, _ := s.CapStoreQuery("c", TableName, "key=?", []any{"k"}, QueryLimit)
	if len(rows) != 1 {
		t.Fatalf("expected 1 chunk after shrink, got %d", len(rows))
	}
	var buf bytes.Buffer
	if err := Get(s, "c", "k", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != small {
		t.Fatalf("content: got %q want %q", buf.String(), small)
	}
}

func TestGet_UnknownKey(t *testing.T) {
	s := newMockStore()
	if err := s.CapStoreCreateTable("c", TableName, schema()); err != nil {
		t.Fatalf("createTable: %v", err)
	}
	var buf bytes.Buffer
	if err := Get(s, "c", "nope", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty, got %q", buf.String())
	}
}

func TestPut_EmptyInput(t *testing.T) {
	s := newMockStore()
	if err := Put(s, "c", "k", strings.NewReader(""), DefaultChunkSize); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rows, _ := s.CapStoreQuery("c", TableName, "key=?", []any{"k"}, QueryLimit)
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows for empty input, got %d", len(rows))
	}
}

func TestPutGet_NonAsciiBytes(t *testing.T) {
	s := newMockStore()
	payload := "Ümlaut-Test: €ñ日本 " + strings.Repeat("αβγ", 20000)
	if err := Put(s, "c", "utf", strings.NewReader(payload), DefaultChunkSize); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var buf bytes.Buffer
	if err := Get(s, "c", "utf", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != payload {
		t.Fatalf("utf mismatch")
	}
}

func TestGet_SortsByChunkIdx(t *testing.T) {
	s := newMockStore()
	if err := s.CapStoreCreateTable("c", TableName, schema()); err != nil {
		t.Fatalf("createTable: %v", err)
	}
	s.CapStoreUpsert("c", TableName, map[string]any{"key": "k", "chunk_idx": 2, "data": "C"})
	s.CapStoreUpsert("c", TableName, map[string]any{"key": "k", "chunk_idx": 0, "data": "A"})
	s.CapStoreUpsert("c", TableName, map[string]any{"key": "k", "chunk_idx": 1, "data": "B"})
	var buf bytes.Buffer
	if err := Get(s, "c", "k", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != "ABC" {
		t.Fatalf("sort order wrong: %q", buf.String())
	}
}

func TestPut_CreateTableIdempotent(t *testing.T) {
	s := newMockStore()
	for i := 0; i < 3; i++ {
		if err := Put(s, "c", fmt.Sprintf("k%d", i), strings.NewReader("x"), DefaultChunkSize); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	rows, _ := s.CapStoreQuery("c", TableName, "", nil, QueryLimit)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
}

// Pins DefaultChunkSize small enough that a single stored chunk fits in the
// MCP JSON-RPC response limit (~25 KB/row). Larger chunks make blobs
// unreadable via mcp__yesmem__cap_store: the row payload trips the MCP
// client's result-size cap and returns an error string instead of JSON.
func TestPut_ChunksStayBelowMCPResponseCap(t *testing.T) {
	const mcpSafeMax = 25000
	s := newMockStore()
	payload := strings.Repeat("x", 200*1024)
	if err := Put(s, "c", "k", strings.NewReader(payload), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rows, _ := s.CapStoreQuery("c", TableName, "key=?", []any{"k"}, QueryLimit)
	if len(rows) == 0 {
		t.Fatalf("expected chunks, got 0")
	}
	for _, row := range rows {
		data, ok := row["data"].(string)
		if !ok {
			t.Fatalf("row data not string: %T", row["data"])
		}
		if len(data) > mcpSafeMax {
			t.Errorf("chunk_idx=%v exceeds MCP-safe size: %d > %d",
				row["chunk_idx"], len(data), mcpSafeMax)
		}
	}
}

// jsonStore pushes every cell through JSON, the way the daemon RPC boundary
// does (rpcStore marshals the upsert payload and the query result). A chunk
// that ends mid-rune is rewritten to U+FFFD on the way through, so bytes that
// were fine in memory come back changed.
type jsonStore struct{ inner *mockStore }

func (j *jsonStore) CapStoreCreateTable(capName, tableName string, columns []storage.ColumnDef) error {
	return j.inner.CapStoreCreateTable(capName, tableName, columns)
}

func (j *jsonStore) CapStoreUpsert(capName, tableName string, data map[string]any) (int64, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return 0, err
	}
	return j.inner.CapStoreUpsert(capName, tableName, decoded)
}

func (j *jsonStore) CapStoreQuery(capName, tableName, where string, args []any, limit int) ([]map[string]any, error) {
	rows, err := j.inner.CapStoreQuery(capName, tableName, where, args, limit)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func (j *jsonStore) CapStoreDelete(capName, tableName, where string, args []any) (int64, error) {
	return j.inner.CapStoreDelete(capName, tableName, where, args)
}

// YM-177/OPS-28: chunks were cut byte-wise, so a boundary could fall inside a
// multi-byte rune. Over the JSON transport that turns the tail into U+FFFD and
// the blob comes back corrupted.
func TestPut_ChunksOnRuneBoundariesAcrossJSON(t *testing.T) {
	const chunkSize = 7
	inner := newMockStore()
	s := &jsonStore{inner: inner}
	payload := strings.Repeat("日本語テスト", 5)

	if err := Put(s, "c", "utf", strings.NewReader(payload), chunkSize); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rows, err := inner.CapStoreQuery("c", TableName, "key=?", []any{"utf"}, QueryLimit)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("want multiple chunks for a %d-byte payload, got %d", len(payload), len(rows))
	}
	for _, row := range rows {
		data, _ := row["data"].(string)
		if !utf8.ValidString(data) {
			t.Fatalf("chunk_idx=%v does not end on a rune boundary: %q", row["chunk_idx"], data)
		}
		if len(data) > chunkSize+utf8.UTFMax-1 {
			t.Errorf("chunk_idx=%v is %d bytes, want <= %d", row["chunk_idx"], len(data), chunkSize+utf8.UTFMax-1)
		}
	}

	var buf bytes.Buffer
	if err := Get(s, "c", "utf", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != payload {
		t.Fatalf("round trip corrupted:\n got %q\nwant %q", buf.String(), payload)
	}
}

// YM-177/OPS-28: Get issued a single query, and CapsQuery clamps the limit to
// 1000 rows — a blob stored in more chunks than that came back silently short.
func TestGet_ReadsPastQueryLimitChunks(t *testing.T) {
	s := newMockStore()
	payload := make([]byte, QueryLimit+250)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	if err := Put(s, "c", "huge", bytes.NewReader(payload), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rows, err := s.CapStoreQuery("c", TableName, "key=?", []any{"huge"}, QueryLimit)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != QueryLimit {
		t.Fatalf("setup: want a full first page of %d chunks, got %d", QueryLimit, len(rows))
	}

	var buf bytes.Buffer
	if err := Get(s, "c", "huge", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.Len() != len(payload) {
		t.Fatalf("read %d bytes, want %d — Get must page past the query limit", buf.Len(), len(payload))
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatal("content mismatch after paging")
	}
}

// The chunk bound is a hard bound. Input that never reaches a rune boundary
// (invalid UTF-8 — the CLI pipes raw stdin) must still be cut at the limit:
// otherwise the chunk grows to the whole payload, which the store refuses past
// its 64 KB cell limit and which is held in memory in full until then.
func TestPut_BoundsChunksForInvalidUTF8(t *testing.T) {
	const chunkSize = 20000
	s := newMockStore()
	payload := bytes.Repeat([]byte{0x80}, chunkSize*2+11)

	if err := Put(s, "c", "bin", bytes.NewReader(payload), chunkSize); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rows, err := s.CapStoreQuery("c", TableName, "key=?", []any{"bin"}, QueryLimit)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("chunks = %d, want 3", len(rows))
	}
	for _, row := range rows {
		data, _ := row["data"].(string)
		if len(data) > chunkSize+utf8.UTFMax-1 {
			t.Errorf("chunk_idx=%v is %d bytes, want <= %d", row["chunk_idx"], len(data), chunkSize+utf8.UTFMax-1)
		}
	}

	var buf bytes.Buffer
	if err := Get(s, "c", "bin", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatal("round trip lost bytes")
	}
}
