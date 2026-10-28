package setup

// Shared fixtures for the setup package: reading and writing the files a setup
// path produces. The three readers this replaces differed only in return type,
// and one of them existed a second time under a ticket name.

import (
	"os"
	"testing"
)

// readFile returns the file's content as a string and fails the test if it
// cannot be read.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// readFileOrFail is readFile for callers that assert on bytes.
func readFileOrFail(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// writeFixtureFile writes a fixture with the permissions a user config has.
func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
