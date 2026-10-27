package sanitize

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// YM-405 (Los 57d): two documentation claims about this package had drifted from
// the code. Both are pinned here against the code itself, so a doc rewrite that
// drifts again fails a test instead of misleading a reader.
const (
	los57dConfigReference = "../../docs/config-reference.md"
	los57dOperationsDoc   = "../../docs/features/operations.md"
)

func los57dReadDoc(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// los57dTemplateKinds is the truth from defaultPatterns: every kind whose
// replacement keeps part of the match (template field set). A kind can be
// registered twice (generic_api_key has a quoted and a bare form), so the set is
// deduplicated: the doc lists kinds, not registrations.
func los57dTemplateKinds() []string {
	seen := make(map[string]struct{})
	var kinds []string
	for _, p := range defaultPatterns() {
		if p.template == "" {
			continue
		}
		if _, ok := seen[p.kind]; ok {
			continue
		}
		seen[p.kind] = struct{}{}
		kinds = append(kinds, p.kind)
	}
	return kinds
}

// The reference doc claimed the template patterns ignore allowed_exceptions and
// left generic_api_key out of the list. replaceTemplate consults the allowlist on
// the full match, so both statements were wrong.
func TestConfigReferenceTemplatePatternsMatchTheCode_Los57d(t *testing.T) {
	doc := los57dReadDoc(t, los57dConfigReference)

	anchor := regexp.MustCompile(`(?i)template patterns \(`)
	loc := anchor.FindStringIndex(doc)
	if loc == nil {
		t.Fatalf("%s: the template pattern sentence is gone", los57dConfigReference)
	}
	start := loc[0]
	rest := doc[start:]
	open := strings.Index(rest, "(")
	if open < 0 {
		t.Fatalf("%s: the template pattern list is unterminated", los57dConfigReference)
	}
	rest = rest[open+1:]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("%s: the template pattern list is unterminated", los57dConfigReference)
	}

	var documented []string
	for _, m := range regexp.MustCompile("`([a-z0-9_]+)`").FindAllStringSubmatch(rest[:end], -1) {
		documented = append(documented, m[1])
	}
	want := los57dTemplateKinds()
	sort.Strings(documented)
	sort.Strings(want)
	if strings.Join(documented, ",") != strings.Join(want, ",") {
		t.Fatalf("documented template patterns %v do not match the code %v", documented, want)
	}

	paragraph := doc[start:]
	if stop := strings.Index(paragraph, "\n\n"); stop > 0 {
		paragraph = paragraph[:stop]
	}
	if regexp.MustCompile(`(?i)\bignore`).MatchString(paragraph) {
		t.Fatalf("the paragraph still claims the template patterns ignore allowed_exceptions: %q", paragraph)
	}
}

// The operations doc claimed the redactor runs at every persistence boundary,
// but every wiring path is gated on secrets_sanitization.enabled, whose default
// is false (daemon.go, llm_client_factory.go, setup.go).
func TestOperationsDocStatesTheSanitizationGate_Los57d(t *testing.T) {
	doc := los57dReadDoc(t, los57dOperationsDoc)

	start := strings.Index(doc, "## 6. Secret Sanitization")
	if start < 0 {
		t.Fatalf("%s: the secret sanitization section is gone", los57dOperationsDoc)
	}
	section := doc[start:]
	if end := strings.Index(section, "### Pattern Kinds"); end > 0 {
		section = section[:end]
	}

	if !strings.Contains(section, "secrets_sanitization.enabled") {
		t.Fatalf("the section states an unconditional redaction and never names the switch: %q", section)
	}
	if !regexp.MustCompile("(?i)defaults? to `false`").MatchString(section) {
		t.Fatalf("the section does not name the disabled default of the switch: %q", section)
	}
}
