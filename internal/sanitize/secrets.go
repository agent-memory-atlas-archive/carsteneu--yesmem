package sanitize

import (
	"regexp"
	"strconv"
	"strings"
)

// Compile-time check: SecretRedactor erfüllt das Sanitizer-Interface.
var _ Sanitizer = (*SecretRedactor)(nil)

// SecretRedactor erkennt typische Secret-Muster (anthropic_api_key,
// openai_api_key, aws_access_key, aws_secret_access_key, github_pat, jwt,
// bearer_token, password_in_url, generic_api_key, ssh_private_key_block,
// gpg_private_key_block, ipv4_public, hex_secret, email, phone) und ersetzt
// sie durch [REDACTED:<kind>]-Marker. Optional kann eine Allowlist von exakten
// Strings übergeben werden, die nicht redacted werden. Die Allowlist prüft den
// vollständigen Regex-Treffer, auch bei den Template-Patterns
// (aws_secret_access_key, password_in_url, generic_api_key), wo nur ein Teil des
// Treffers ersetzt würde. Ein allowlisteter Treffer wird für die restlichen
// Patterns aus dem Text genommen und am Ende wieder eingesetzt: sonst redigiert
// ein späteres, überlappendes Pattern einen Teil davon trotzdem, etwa das
// email-Pattern den password@host-Teil einer allowlisteten URL.
type SecretRedactor struct {
	allowed  map[string]struct{}
	patterns []redactPattern
}

type redactPattern struct {
	kind     string
	re       *regexp.Regexp
	template string              // optional Expand-style Replacement; leer => Default-Marker [REDACTED:<kind>].
	// filter is invoked on each regex match. Return true to redact the match,
	// false to keep it unmodified in the output. nil means always redact.
	filter func(match string) bool
}

// NewSecretRedactor returns a Sanitizer that applies the default 15-kind pattern set:
// anthropic_api_key, openai_api_key, aws_access_key, aws_secret_access_key, github_pat,
// jwt, bearer_token, password_in_url, generic_api_key, ssh_private_key_block,
// gpg_private_key_block, ipv4_public, hex_secret, email, phone.
//
// allowed contains exact strings that will NOT be redacted. The comparison is against
// the FULL regex match string for the triggered pattern, not against substrings or
// capture groups.
//
// Three patterns in the default set use template-style replacement: they capture
// surrounding context as part of their match and substitute only part of it.
//
//   - aws_secret_access_key: match includes the keyword+separator preamble
//     (e.g. "aws_secret_access_key=THEKEY40CHARS"), not the bare key alone
//   - password_in_url: match includes the scheme://user: prefix and @host suffix
//     (e.g. "https://user:thepassword@host"), not the bare password alone
//   - generic_api_key: match includes keyword, separator and the opening quote
//     (e.g. `"api_key": "THESECRET"`), not the bare value — replacing the whole
//     assignment would swallow the closing quote of a JSON string. A value
//     without quotes is a second pattern of the same kind whose marker is emitted
//     quoted, so both forms leave valid JSON behind.
//
// Template-style patterns respect the allowlist on the full match too: adding the
// exact matched string to allowed skips the replacement, although only the secret
// part of the match would have been substituted out.
//
// All other patterns (anthropic_api_key, openai_api_key, aws_access_key, github_pat,
// jwt, bearer_token, ssh_private_key_block, gpg_private_key_block,
// ipv4_public, hex_secret, email, phone) are plain matches: adding the exact matched
// string to allowed skips redaction for that value.
func NewSecretRedactor(allowed []string) *SecretRedactor {
	allow := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		allow[a] = struct{}{}
	}
	return &SecretRedactor{
		allowed:  allow,
		patterns: defaultPatterns(),
	}
}

// defaultPatterns returns the compiled set of secret-detection patterns. Kinds in registration order:
// anthropic_api_key, openai_api_key, aws_access_key, aws_secret_access_key,
// github_pat, jwt, bearer_token, password_in_url, generic_api_key,
// ssh_private_key_block, gpg_private_key_block, ipv4_public, hex_secret,
// email, phone.
func defaultPatterns() []redactPattern {
	return []redactPattern{
		{kind: "anthropic_api_key", re: regexp.MustCompile(`sk-ant-[a-zA-Z0-9_\-]{50,}`)},
		// openai_api_key MUST stay registered after anthropic_api_key: sk-ant-* keys
		// match both, and order decides which kind tag wins.
		{kind: "openai_api_key", re: regexp.MustCompile(`sk-(?:proj-|svcacct-|admin-|None-)?[A-Za-z0-9_-]{20,}`)},
		{kind: "aws_access_key", re: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
		{kind: "aws_secret_access_key", re: regexp.MustCompile(`(?i)(aws[_\-]?secret[_\-]?access[_\-]?key["']?\s*[:=]\s*["']?)([A-Za-z0-9/+]{40})`), template: "${1}[REDACTED:aws_secret_access_key]"},
		{kind: "github_pat", re: regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{36,}\b`)},
		{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`)},
		{kind: "bearer_token", re: regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-+/=]{8,}`)},
		{kind: "password_in_url", re: regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+\-.]*://[^:/@\s]*:)([^@\s]+)(@[^/\s]+)`), template: "${1}[REDACTED:password_in_url]${3}"},
		// generic_api_key keeps the keyword, the separator and the opening quote in
		// the output: replacing the whole assignment swallowed the closing quote of
		// a JSON string and left {"[REDACTED:generic_api_key]} behind, which is not
		// valid JSON. Quoted values go first; for a value without quotes the marker
		// is emitted inside quotes, so that a bare value in a JSON body
		// ({"token": 12345678901234567890}) stays parseable too.
		//
		// The value is opaque, not a charset run: the old class stopped at the first
		// character outside [A-Za-z0-9._\-+/=], so a JSON-escaped value (a literal
		// backslash-n inside the string) or a value with a colon went out in the
		// clear. An escaped quote (\" inside a JSON string) is the same case: the
		// pattern accepts backslash pairs as one unit, otherwise it stops at the
		// backslash and the whole assignment stays unredacted. Only the quote,
		// backslash and line ends bound the value; every other character counts, so
		// a value with brackets or separators is redacted whole. The 20-char minimum
		// still applies.
		{kind: "generic_api_key", re: regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|secret|token)["']?\s*[:=]\s*["'])((?:\\.|[^"\\\r\n]){20,})`), template: "${1}[REDACTED:generic_api_key]"},
		{kind: "generic_api_key", re: regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|secret|token)["']?\s*[:=]\s*)([^\s"'\[\],;}]{20,})`), template: "${1}\"[REDACTED:generic_api_key]\""},
		{kind: "ssh_private_key_block", re: regexp.MustCompile(`(?s)-----BEGIN (?:OPENSSH |RSA |DSA |EC |)PRIVATE KEY-----.*?-----END (?:OPENSSH |RSA |DSA |EC |)PRIVATE KEY-----`)},
		{kind: "gpg_private_key_block", re: regexp.MustCompile(`(?s)-----BEGIN PGP PRIVATE KEY BLOCK-----.*?-----END PGP PRIVATE KEY BLOCK-----`)},
		{kind: "ipv4_public", re: regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1\d{2}|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d{2}|[1-9]?\d)\b`), filter: filterPublicIPv4},
		{kind: "hex_secret", re: regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)},
		{kind: "email", re: regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)},
		{kind: "phone", re: regexp.MustCompile(`\+?\d{1,3}[\s.-]?\(?\d{2,4}\)?[\s.-]?\d{3,4}[\s.-]?\d{4}\b`)},
	}
}

// Sanitize wendet alle Pattern an und ersetzt Treffer durch [REDACTED:<kind>],
// es sei denn der Treffer steht exakt in der Allowlist. Template-Patterns
// (aws_secret_access_key, password_in_url, generic_api_key) verwenden
// Regex-Backrefs zum Erhalten von Capture-Group-Inhalten; die Allowlist wird
// auch dort auf Basis des vollständigen Treffers geprüft.
//
// Ein allowlisteter Treffer wird bis zum Ende der Pattern-Kette durch einen
// Sentinel ersetzt und danach zurückgeschrieben. Ohne diesen Schritt kann ein
// späteres Pattern einen Teil des Treffers trotzdem entfernen: password_in_url
// deckt in "https://user:pw@host.tld" nur den Passwortteil ab, das danach
// laufende email-Pattern trifft "pw@host.tld" erneut und redigiert es, obwohl
// die URL allowlistet ist.
func (r *SecretRedactor) Sanitize(s string) string {
	if len(r.allowed) == 0 {
		return r.applyAll(s, nil)
	}
	var kept []string
	s = r.applyAll(s, func(match string) (string, bool) {
		if _, ok := r.allowed[match]; !ok {
			return "", false
		}
		kept = append(kept, match)
		return keepSentinel(len(kept) - 1), true
	})
	return restoreSentinels(s, kept)
}

const keepSentinelPrefix = "\x00keep"

// keepSentinel returns the placeholder that shields an allowlisted match from
// the remaining patterns. The placeholder never forms a pattern of its own
// (no default pattern matches a NUL-delimited word). A pattern can still match
// it together with surrounding text, and then the allowlisted text is lost —
// over-redaction, never a leak.
func keepSentinel(idx int) string {
	return keepSentinelPrefix + strconv.Itoa(idx) + "\x00"
}

// restoreSentinels puts the allowlisted matches back in one pass. A
// strings.ReplaceAll per index is quadratic: with thousands of allowlisted
// matches in one session text it dominated the call.
func restoreSentinels(s string, kept []string) string {
	if len(kept) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for {
		start := strings.Index(s, keepSentinelPrefix)
		if start < 0 {
			b.WriteString(s)
			return b.String()
		}
		rel := strings.IndexByte(s[start+1:], 0)
		if rel < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := start + 1 + rel
		idx, err := strconv.Atoi(s[start+len(keepSentinelPrefix) : end])
		if err != nil || idx < 0 || idx >= len(kept) {
			b.WriteString(s[:end+1])
			s = s[end+1:]
			continue
		}
		b.WriteString(s[:start])
		b.WriteString(kept[idx])
		s = s[end+1:]
	}
}

// applyAll runs every pattern in registration order. keep, when set, gets the
// full match first and can replace it (an allowlisted match becomes a sentinel);
// a nil keep means no allowlist is configured.
func (r *SecretRedactor) applyAll(s string, keep func(match string) (string, bool)) string {
	for _, p := range r.patterns {
		if p.template != "" {
			s = replaceTemplate(s, p, keep)
			continue
		}
		s = p.re.ReplaceAllStringFunc(s, func(match string) string {
			if keep != nil {
				if repl, ok := keep(match); ok {
					return repl
				}
			}
			if p.filter != nil && !p.filter(match) {
				return match
			}
			return "[REDACTED:" + p.kind + "]"
		})
	}
	return s
}

// replaceTemplate applies a pattern whose replacement keeps part of the match.
// The allowlist is consulted on the full match, so a template pattern can be
// exempted just like a plain one — only the replacement is partial.
func replaceTemplate(s string, p redactPattern, keep func(match string) (string, bool)) string {
	var b []byte
	last := 0
	replaced := false
	for _, m := range p.re.FindAllStringSubmatchIndex(s, -1) {
		repl, exempt := "", false
		if keep != nil {
			repl, exempt = keep(s[m[0]:m[1]])
		}
		if !exempt {
			repl = string(p.re.Expand(nil, []byte(p.template), []byte(s), m))
		}
		b = append(b, s[last:m[0]]...)
		b = append(b, repl...)
		last = m[1]
		replaced = true
	}
	if !replaced {
		return s
	}
	return string(append(b, s[last:]...))
}

// Name implementiert Sanitizer.
func (r *SecretRedactor) Name() string {
	return "secret_redactor"
}

// filterPublicIPv4 returns true for public IPs that should be redacted and
// false for RFC-1918 / loopback / link-local ranges that should be preserved.
func filterPublicIPv4(match string) bool {
	parts := strings.Split(match, ".")
	if len(parts) != 4 {
		return true
	}
	// regex byte-range alternation guarantees 0..255 per octet; Atoi cannot fail here.
	a, _ := strconv.Atoi(parts[0])
	b, _ := strconv.Atoi(parts[1])
	switch {
	case a == 0:
		return false
	case a == 10:
		return false
	case a == 100 && b >= 64 && b <= 127:
		return false
	case a == 127:
		return false
	case a == 169 && b == 254:
		return false
	case a == 172 && b >= 16 && b <= 31:
		return false
	case a == 192 && b == 168:
		return false
	case a >= 224:
		return false
	}
	return true
}
