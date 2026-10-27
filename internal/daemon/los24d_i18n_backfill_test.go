package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/extraction"
)

// YM-338 (Los 24d): new strings.yaml keys reached an existing installation only
// through a setup run. The daemon backfills them at startup, translated into the
// language the file already uses.
func TestBackfillStringsOnStartupTranslatesOnlyMissingKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "strings.yaml")
	if err := os.WriteFile(path, []byte("greeting: Hallo\ntool_hint: Nutze diese Werkzeuge.\n"), 0o644); err != nil {
		t.Fatalf("write strings: %v", err)
	}

	client := &los24dBackfillClient{response: "metamemory_header: \"Meine Wissensqualitaet hier:\"\ngreeting: HIJACKED\n"}
	if err := backfillStringsOnStartup(dir, client); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if client.prompt == "" {
		t.Fatal("the backfill never called the model, so no translation happened")
	}
	if !strings.Contains(client.prompt, "metamemory_header") {
		t.Errorf("the prompt does not name the missing key:\n%s", client.prompt)
	}
	if !strings.Contains(client.prompt, "Hallo") {
		t.Errorf("the prompt does not carry the file's language sample:\n%s", client.prompt)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read strings: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "greeting: Hallo") {
		t.Errorf("an existing translation was overwritten:\n%s", text)
	}
	if strings.Contains(text, "HIJACKED") {
		t.Errorf("a key that already existed was replaced:\n%s", text)
	}
	if !strings.Contains(text, "Meine Wissensqualitaet hier:") {
		t.Errorf("the translated key is missing:\n%s", text)
	}
}

// A missing file belongs to setup: without a file there is no language to match,
// and the backfill must not create one in English.
func TestBackfillStringsOnStartupLeavesAMissingFileAlone(t *testing.T) {
	dir := t.TempDir()
	client := &los24dBackfillClient{}
	if err := backfillStringsOnStartup(dir, client); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if client.prompt != "" {
		t.Error("the backfill called the model although no strings file exists")
	}
	if _, err := os.Stat(filepath.Join(dir, "strings.yaml")); !os.IsNotExist(err) {
		t.Errorf("the backfill created a strings file (stat err = %v)", err)
	}
}

// Without a client there is nothing to translate with; the daemon must start
// anyway.
func TestBackfillStringsOnStartupWithoutAClientIsANoOp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "strings.yaml"), []byte("greeting: Hallo\n"), 0o644); err != nil {
		t.Fatalf("write strings: %v", err)
	}
	if err := backfillStringsOnStartup(dir, nil); err != nil {
		t.Fatalf("backfill without a client: %v", err)
	}
}

type los24dBackfillClient struct {
	prompt   string
	response string
}

func (c *los24dBackfillClient) Complete(system, user string, opts ...extraction.CallOption) (string, error) {
	c.prompt = user
	return c.response, nil
}

func (c *los24dBackfillClient) CompleteJSON(system, user string, schema map[string]any, opts ...extraction.CallOption) (string, error) {
	c.prompt = user
	return c.response, nil
}

func (c *los24dBackfillClient) Name() string  { return "los24d-backfill" }
func (c *los24dBackfillClient) Model() string { return "los24d-backfill" }

var _ extraction.LLMClient = (*los24dBackfillClient)(nil)
