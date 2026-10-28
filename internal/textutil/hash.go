package textutil

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

var whitespaceRE = regexp.MustCompile(`\s+`)

// NormalizeForHash normalizes text for content hashing:
// lowercase, collapse all whitespace to single space, trim.
func NormalizeForHash(text string) string {
	lower := strings.ToLower(text)
	collapsed := whitespaceRE.ReplaceAllString(lower, " ")
	return strings.TrimSpace(collapsed)
}

// ContentHash returns the hex-encoded SHA-256 of the normalized text.
func ContentHash(text string) string {
	normalized := NormalizeForHash(text)
	h := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("%x", h)
}

// BodyDigest is the provenance digest of a CAP.md body. Unlike ContentHash it does
// not fold case or whitespace: inside a bash or python body "$URL" and "$url",
// "True" and "true", are different programs, and a re-indented body is a different
// program too. Only what no parser sees is normalized away — the line endings and
// the trailing newlines an editor leaves behind.
func BodyDigest(body []byte) string {
	normalized := strings.ReplaceAll(string(body), "\r\n", "\n")
	normalized = strings.TrimRight(normalized, "\n")
	h := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("%x", h)
}
