package codescan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// countingScanner counts Scan calls and can be told to fail.
type countingScanner struct {
	calls int
	err   error
}

func (c *countingScanner) Scan(rootDir string) (*ScanResult, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return &ScanResult{RootDir: rootDir}, nil
}

// memFailureStore implements ScanStore + ScanFailureStore in memory.
type memFailureStore struct {
	failures map[string]time.Time
}

func (m *memFailureStore) LoadScan(project string) (string, string, int64, error) { return "", "", 0, nil }
func (m *memFailureStore) PersistScan(project, scanJSON, gitHead string, cbmMtime int64) error {
	return nil
}
func (m *memFailureStore) RecordScanFailure(project, errMsg string) error {
	m.failures[project] = time.Now()
	return nil
}
func (m *memFailureStore) GetScanFailure(project string) (time.Time, string, error) {
	failedAt, ok := m.failures[project]
	if !ok {
		return time.Time{}, "", nil
	}
	return failedAt, "boom", nil
}
func (m *memFailureStore) ClearScanFailure(project string) error {
	delete(m.failures, project)
	return nil
}

// scanOnlyStore implements only ScanStore (no ScanFailureStore).
type scanOnlyStore struct{}

func (s *scanOnlyStore) LoadScan(project string) (string, string, int64, error) {
	return "", "", 0, nil
}
func (s *scanOnlyStore) PersistScan(project, scanJSON, gitHead string, cbmMtime int64) error {
	return nil
}

func gitWorktreeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644)
	os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte("abc123\n"), 0644)
	return dir
}

func TestScanCooldown_BlocksRetryAfterFailure(t *testing.T) {
	dir := gitWorktreeDir(t)
	fs := &memFailureStore{failures: map[string]time.Time{}}
	inner := &countingScanner{err: errors.New("cbm cli: signal: killed")}
	cs := NewCachedScanner(inner).WithStore(fs)

	if _, err := cs.Scan(dir); err == nil {
		t.Fatal("first scan should fail")
	}
	if inner.calls != 1 {
		t.Fatalf("inner scan calls = %d, want 1", inner.calls)
	}

	_, err := cs.Scan(dir)
	if !errors.Is(err, ErrScanCooldown) {
		t.Fatalf("second scan error = %v, want ErrScanCooldown", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner scan calls after cooldown block = %d, want 1", inner.calls)
	}
}

func TestScanCooldown_ExpiresAfterCooldown(t *testing.T) {
	dir := gitWorktreeDir(t)
	fs := &memFailureStore{failures: map[string]time.Time{
		projectKey(dir): time.Now().Add(-11 * time.Minute),
	}}
	inner := &countingScanner{}
	cs := NewCachedScanner(inner).WithStore(fs)

	res, err := cs.Scan(dir)
	if err != nil {
		t.Fatalf("scan after cooldown expiry: %v", err)
	}
	if res == nil {
		t.Fatal("expected scan result after cooldown expiry")
	}
	if inner.calls != 1 {
		t.Fatalf("inner scan calls = %d, want 1", inner.calls)
	}
}

func TestScanCooldown_ClearedOnSuccess(t *testing.T) {
	dir := gitWorktreeDir(t)
	fs := &memFailureStore{failures: map[string]time.Time{
		projectKey(dir): time.Now().Add(-11 * time.Minute),
	}}
	inner := &countingScanner{}
	cs := NewCachedScanner(inner).WithStore(fs)

	failedAt, _, _ := fs.GetScanFailure(projectKey(dir))
	if failedAt.IsZero() {
		t.Fatal("expected seeded failure record")
	}

	if _, err := cs.Scan(dir); err != nil {
		t.Fatalf("scan: %v", err)
	}

	failedAt, _, err := fs.GetScanFailure(projectKey(dir))
	if err != nil {
		t.Fatalf("GetScanFailure: %v", err)
	}
	if !failedAt.IsZero() {
		t.Fatal("failure record should be cleared after successful scan")
	}
}

func TestScanCooldown_NoFailureStoreRetries(t *testing.T) {
	dir := gitWorktreeDir(t)
	inner := &countingScanner{err: errors.New("boom")}
	cs := NewCachedScanner(inner).WithStore(&scanOnlyStore{})

	if _, err := cs.Scan(dir); err == nil {
		t.Fatal("first scan should fail")
	}
	if _, err := cs.Scan(dir); err == nil {
		t.Fatal("second scan should fail (no failure store = no cooldown)")
	}
	if inner.calls != 2 {
		t.Fatalf("inner scan calls = %d, want 2", inner.calls)
	}
}
