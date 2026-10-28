package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newChildFixtureDB builds a temp opencode.db with one fresh child and one stale
// child. sqlite3 CLI only, mirroring the production access path.
func newChildFixtureDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	run := func(query string) {
		t.Helper()
		if out, err := exec.Command("sqlite3", dbPath, query).CombinedOutput(); err != nil {
			t.Fatalf("sqlite3 %q: %v (%s)", query, err, out)
		}
	}
	now := time.Now().UnixMilli()
	run("CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, time_updated INTEGER)")
	run("INSERT INTO session VALUES ('child-fresh', 'p-fresh', " + strconv.FormatInt(now, 10) + ")")
	run("INSERT INTO session VALUES ('child-stale', 'p-stale', " + strconv.FormatInt(now-10*60*1000, 10) + ")")
	run("INSERT INTO session VALUES ('child-quote', 'p''fresh', " + strconv.FormatInt(now, 10) + ")")
	// A value with the two characters the dot-command needs a backslash for.
	run(`INSERT INTO session VALUES ('child-escaped', 'q"z' || char(92) || 'w', ` + strconv.FormatInt(now, 10) + `)`)
	return dbPath
}

// TestQueryFirstSessionRowParams_BindsValues pins the parameterised transport
// (YM-216): the caller's value travels as a sqlite3 binding, so SQL syntax inside
// it stays data. The apostrophe row proves the value is not mangled, the tautology
// value proves it cannot widen the predicate.
func TestQueryFirstSessionRowParams_BindsValues(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	dbPath := newChildFixtureDB(t)
	ctx := context.Background()
	query := func(table string) string {
		return "SELECT id FROM " + table + " WHERE parent_id = @parent LIMIT 1"
	}

	row, err := queryFirstSessionRowParams(ctx, dbPath, query, []sessionQueryParam{{name: "@parent", value: "p-fresh"}})
	if err != nil {
		t.Fatalf("bound query: %v", err)
	}
	if row != "child-fresh" {
		t.Fatalf("row = %q, want child-fresh", row)
	}

	// A value that is also SQL text must match only itself.
	row, err = queryFirstSessionRowParams(ctx, dbPath, query, []sessionQueryParam{{name: "@parent", value: "p-fresh' OR '1'='1"}})
	if err != nil {
		t.Fatalf("bound query with tautology value: %v", err)
	}
	if row != "" {
		t.Fatalf("row = %q, want no row: the tautology value must not widen the predicate", row)
	}

	// An apostrophe inside the value is data, not the end of a literal.
	row, err = queryFirstSessionRowParams(ctx, dbPath, query, []sessionQueryParam{{name: "@parent", value: "p'fresh"}})
	if err != nil {
		t.Fatalf("bound query with apostrophe value: %v", err)
	}
	if row != "child-quote" {
		t.Fatalf("row = %q, want child-quote", row)
	}

	// The characters the dot-command needs a backslash for must round-trip
	// unchanged through the real CLI, not just through the escaping helper.
	row, err = queryFirstSessionRowParams(ctx, dbPath, query, []sessionQueryParam{{name: "@parent", value: `q"z\w`}})
	if err != nil {
		t.Fatalf("bound query with quote and backslash: %v", err)
	}
	if row != "child-escaped" {
		t.Fatalf("row = %q, want child-escaped", row)
	}
}

// TestOpencodeChildSessionActive_BindsTheSessionID is the regression guard for the
// injected session id: the production query may not carry the caller's value as
// SQL text.
func TestOpencodeChildSessionActive_BindsTheSessionID(t *testing.T) {
	src, err := os.ReadFile("opencode_children.go")
	if err != nil {
		t.Fatalf("read opencode_children.go: %v", err)
	}
	if strings.Contains(string(src), `parent_id = '%s'`) {
		t.Fatalf("opencode_children.go still splices the parent session id into the SQL text (want a bound @parent parameter)")
	}
	if !strings.Contains(string(src), "sessionQueryParam") {
		t.Fatalf("opencode_children.go does not use the bound query path (sessionQueryParam)")
	}
}

// TestOpencodeChildSessionActive_CraftedIDStaysInert keeps the behavioural side of
// the same finding: a crafted parent id may not report an active child.
func TestOpencodeChildSessionActive_CraftedIDStaysInert(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	dbPath := newChildFixtureDB(t)
	for _, crafted := range []string{
		"x' OR 1=1 --",
		"x' OR '1'='1",
		"x'; DROP TABLE session; --",
	} {
		if opencodeChildSessionActive(dbPath, crafted) {
			t.Errorf("crafted parent id %q reported an active child", crafted)
		}
	}
}
