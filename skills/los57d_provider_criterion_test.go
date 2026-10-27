package skills

import (
	"regexp"
	"strings"
	"testing"
)

// YM-405 (Los 57d): a bare model name is resolved through buildModelProviderMap,
// and sortProviders decides which provider wins an ambiguous name: credential
// backed providers first, ProviderID alphabetically only as the tiebreak
// (internal/proxy/provider_autoconf.go). The rule is shipped twice, as a skill
// and as the opencode command file, so both copies are pinned here: a fix in one
// of them leaves the other telling agents the wrong thing.
const (
	los57dProviderAnchor      = "**Coding-optimized variants**"
	los57dOpencodeCommandCopy = "bundled-commands/opencode/yesloop.md"
)

func TestEveryShippedYesloopCopyNamesTheCredentialPrecedence_Los57d(t *testing.T) {
	for _, path := range []string{yesloopSkillPath, los57dOpencodeCommandCopy} {
		path := path
		t.Run(path, func(t *testing.T) {
			text := readBundled(t, path)

			start := strings.Index(text, los57dProviderAnchor)
			if start < 0 {
				t.Fatalf("%s: the model resolution paragraph is gone", path)
			}
			paragraph := text[start:]
			if end := strings.Index(paragraph, "\n\n"); end > 0 {
				paragraph = paragraph[:end]
			}

			if !regexp.MustCompile(`(?i)credential-backed[^.]{0,120}\b(wins?|first|before|over)\b`).MatchString(paragraph) {
				t.Fatalf("%s: the paragraph does not state that the credential-backed provider wins: %q", path, paragraph)
			}
			if !regexp.MustCompile(`(?i)alphabetical`).MatchString(paragraph) {
				t.Fatalf("%s: the paragraph lost the alphabetical tiebreak: %q", path, paragraph)
			}
		})
	}
}
