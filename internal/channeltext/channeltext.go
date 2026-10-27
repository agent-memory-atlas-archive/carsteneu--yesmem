// Package channeltext renders the parts of a channel message that a receiving
// agent reads as structure: the sender label, the message type and the header
// lines built from them.
//
// The proxy renders these lines when it injects a channel message into a turn,
// and the daemon renders the same message on the opencode API transport. Both
// transports deliver to the same reader, so the sanitizing rules live here once
// instead of once per transport.
package channeltext

import (
	"strings"
	"unicode"
)

// MaxFieldLen bounds a caller-supplied value that is rendered into a message
// header.
const MaxFieldLen = 64

// Field flattens, bounds and de-controls a caller-supplied value for the message
// header. The receiving agent reads these lines as structure, so a newline
// inside one of them would forge a second header line — and, on the
// message-type line, a whole forged directive. C0/C1 controls and the bidi
// overrides are dropped: a label needs none of them.
func Field(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || r == '\u2028' || r == '\u2029':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		case r == 0x061c || r == 0x200e || r == 0x200f:
			return -1
		case r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > MaxFieldLen {
		return string(runes[:MaxFieldLen]) + "…"
	}
	return s
}

// SenderLabel renders the sender for the message header. The sender of a channel
// message is asserted by its caller, so it is untrusted on every transport.
func SenderLabel(sender string) string {
	if label := Field(sender); label != "" {
		return label
	}
	return "unknown"
}

// headerOpener begins the header line of a rendered channel message. The
// receiving agent attributes a message by that line.
const headerOpener = "📨 Von "

// escapedOpener replaces the opener inside a body line. The escape is visible on
// purpose: a reader has to see that the line is text the sender wrote, not a
// message line of its own. The header word stays readable ("📨\ Von …",
// "📨\ DIALOG …"), the glyph-word sequence that makes a reader split the turn is
// broken.
const escapedOpener = "📨\\ "

// Body renders a stored message body for injection. Stored content can carry
// escaped newlines (a caller writing a literal \n), so they are opened up first
// — on both transports, so the same message reads the same way. Afterwards no
// body line may open like the header: the body is text a caller wrote, and a
// caller must not be able to append a second, differently attributed message to
// the turn it was delivered in.
func Body(content string) string {
	return Neutralise(strings.ReplaceAll(content, `\n`, "\n"))
}

// Neutralise defangs body lines that a reader would take for the message
// header, without touching escaped sequences. Use it for content that is
// already rendered as written: opening escaped newlines there would cut a
// Windows path or a regex apart.
func Neutralise(content string) string {
	// Carriage return, vertical tab, form feed, NEL and the Unicode separators
	// are line breaks to a reader even though a split on "\n" does not see
	// them, so a header could hide behind one.
	content = lineBreakReplacer.Replace(content)

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = neutraliseHeaderLine(line)
	}
	return strings.Join(lines, "\n")
}

var lineBreakReplacer = strings.NewReplacer(
	"\r\n", "\n",
	"\r", "\n",
	"\v", "\n",
	"\f", "\n",
	"\u0085", "\n",
	"\u2028", "\n",
	"\u2029", "\n",
)

// escapedSessionWord replaces the word that opens the session line. The escape
// is visible on purpose, like the header escape: a reader still reads the word,
// but the line no longer opens with the marker that names the reply address.
const escapedSessionWord = `SESSION\_ID`

// neutraliseHeaderLine defangs a line a reader would take for the message
// structure: the header ("📨 Von …", "📨 DIALOG …") and the session line
// ("SESSION_ID: …") that the injected block carries above it. The line is
// rebuilt from its visible form, so the characters that only hide the
// resemblance (variation selectors, zero-width joiners, soft hyphens, combining
// marks) do not survive into the escaped version.
func neutraliseHeaderLine(line string) string {
	// The indent keeps a quote marker: visible() drops ">" for the comparison, and
	// rebuilding the line without it would move quoted text to column 0.
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t>"))]
	rest, ok := headerRemainder(line[len(indent):])
	if !ok {
		if sessionRest, isSession := sessionLineRemainder(line[len(indent):]); isSession {
			return indent + escapedSessionWord + ":" + sessionRest
		}
		return line
	}
	return indent + escapedOpener + rest
}

// headerWords are the words a rendered message line carries right after the
// glyph: the injected channel message ("📨 Von <sender>") and the dialog line
// ("📨 DIALOG [<topic>] …"). Both are structure to the reader —
// internal/extraction/chunker.go splits a turn on either — so a body line may not
// open with either.
var headerWords = []string{"von", "dialog"}

// headerRemainder returns what follows the opener when the line opens like a
// message header. The comparison reads what a reader sees, not the exact bytes:
// case, invisible format characters, combining marks, any kind of space and the
// fullwidth forms are transparent, because the receiving agent reads a meaning
// and not a byte string.
func headerRemainder(line string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimLeft(visible(line), " "), "📨")
	if !ok {
		return "", false
	}
	rest = strings.TrimLeft(rest, " \t")
	word := rest
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		word = rest[:i]
	}
	// "Von" is a word of its own ("VonWeise" is not a header), while the dialog
	// form is matched as a prefix because that is how the reader matches it: any
	// line starting with the dialog word marks the turn as injected.
	if strings.EqualFold(word, headerWords[0]) {
		return rest, true
	}
	if len(rest) >= len(headerWords[1]) && strings.EqualFold(rest[:len(headerWords[1])], headerWords[1]) {
		return rest, true
	}
	return "", false
}

// sessionLineRemainder returns what follows the colon when the line opens like
// the session line the injected block carries ("SESSION_ID: <address>"), read
// the way a reader sees it: case, spacing and invisible characters are
// transparent. The address names the reply target, so a body line that opens
// like this one would redirect the answer.
func sessionLineRemainder(line string) (string, bool) {
	const marker = "session_id"
	flat := strings.TrimLeft(visible(line), " ")
	if len(flat) < len(marker) || !strings.EqualFold(flat[:len(marker)], marker) {
		return "", false
	}
	rest := strings.TrimLeft(flat[len(marker):], " \t")
	if !strings.HasPrefix(rest, ":") {
		return "", false
	}
	// The remainder is passed on as it reads, spaces included: the escape marks
	// the line, it must not rewrite the text it defends against.
	value := rest[1:]
	// Only an address-shaped value makes the line a reply target. A sentence
	// ("session_id: the id of this session") or a prose fragment is not one, and
	// escaping it would mangle legitimate body text for nothing.
	trimmed := strings.TrimLeft(value, " \t")
	if trimmed == "" || len(trimmed) > MaxFieldLen || strings.ContainsAny(trimmed, " \t") {
		return "", false
	}
	return value, true
}

// visible reduces a line to what a reader actually sees: quote markers and
// indent are dropped, every kind of space becomes a plain one, the fullwidth
// forms are folded, and the characters that carry no glyph (format characters
// such as the soft hyphen, BOM or zero-width joiner, and combining marks) are
// removed. A disguise made of them then reads the same way it would be
// rendered.
func visible(line string) string {
	line = strings.TrimLeftFunc(line, func(r rune) bool { return r == '>' || r == ' ' || r == '\t' })

	var b strings.Builder
	b.Grow(len(line))
	for _, r := range line {
		switch {
		case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r):
			continue
		case unicode.Is(unicode.Zs, r), r == '\t':
			b.WriteRune(' ')
			continue
		case r >= 0xFF01 && r <= 0xFF5E:
			// Fullwidth ASCII reads like the ASCII it stands for.
			b.WriteRune(r - 0xFEE0)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// MessageType renders the message type for the header. Only the documented types
// are shown; anything else reads as "command" — the same rule the reply
// directive follows, so the header and the instruction can never disagree and a
// type the caller invented cannot appear in a line the receiving agent reads as
// structure.
func MessageType(msgType string) string {
	switch msgType {
	case "command", "response", "ack", "status":
		return msgType
	}
	return "command"
}
