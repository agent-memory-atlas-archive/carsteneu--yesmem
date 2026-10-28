package storage

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/carsteneu/yesmem/internal/models"
)

// Commit-triggered resolution (YM-260).
//
// The commit hook, the CLI and the v2 plugin used to resolve the first hit of an
// OR search, with no threshold at all: that is how learning #66092 was closed by
// an unrelated Los-11 commit. Los 11c then required a unique candidate that
// contains every query word, which no realistic commit satisfies (778 of 784
// historical commit resolutions would have been refused).
//
// The rule landed here is the deterministic one: a commit closes an open task
// only when its SUBJECT names that task, either by learning id or by ticket key.
//
// A graded word-coverage fallback was implemented and measured first, and it was
// dropped: replayed against the 150 most recent subjects of this repository and
// the 500 open tasks of its project, it resolved exactly twice and both were
// wrong ("Merge commit '00f957c9' into yesloop/los-19" hit an unrelated
// orchestrator task at coverage 0.5; a commit naming YM-167 lost its ticket token
// to the digit filter and hit an interval-seconds task at 0.556). Prose overlap
// is not evidence in this repository, so it is not used as a resolve basis. An
// agent that wants to close a task from a commit writes the ticket key or the
// learning id into the subject, or calls resolve(learning_id) explicitly.
const (
	// maxCommitMessageBytes keeps a pathological request out of the regexes and
	// the tokenizer.
	maxCommitMessageBytes = 8192
	// maxAnchorsPerSubject bounds the lookups one subject may trigger. More
	// references than this cannot be told apart, so the subject is refused.
	maxAnchorsPerSubject = 8
	// minAnchorSharedTerms is the overlap an id anchor needs: "#123" alone is
	// also issue syntax, and one shared word is what closed #66092 by accident.
	minAnchorSharedTerms = 2
	// maxCommitCandidates is how many anchored items an ambiguous subject may
	// report for the caller's log line.
	maxCommitCandidates = 5
	// keyLookupLimit bounds the SQL prefilter for a ticket key. A full window is
	// treated as unknowable, never as unique.
	keyLookupLimit = 200
	// commitReasonPrefix marks a resolution that a commit performed.
	commitReasonPrefix = "git commit: "
	// commitReasonMaxLen keeps the audit trail in supersede_reason short.
	commitReasonMaxLen = 200
)

var (
	learningIDAnchorRe = regexp.MustCompile(`#(\d{1,9})\b`)
	ticketKeyAnchorRe  = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-[0-9]{1,5}\b`)
)

// technicalKeyPrefixes are uppercase-dash-digit shapes that are not tickets:
// standards and model names. Without this list an ordinary subject like
// "fix: compare SHA-256 digests" would anchor the one open item that mentions
// SHA-256 and close it with no further evidence.
var technicalKeyPrefixes = map[string]bool{
	"UTF": true, "SHA": true, "MD": true, "HTTP": true, "HTTPS": true, "ISO": true,
	"AES": true, "RSA": true, "DES": true, "GPT": true, "GLM": true, "LLM": true,
	"CVE": true, "CWE": true, "TLS": true, "SSL": true, "TCP": true, "UDP": true,
	"DNS": true, "API": true, "CPU": true, "GPU": true, "USB": true, "PCI": true,
	"SATA": true, "NVME": true, "UTC": true, "GMT": true, "ECC": true, "DER": true,
	"PEM": true, "AMD": true, "LTS": true,
}

// CommitResolve is the outcome of matching a commit message against open tasks.
// Target is nil whenever nothing may be resolved; Reason then says why.
type CommitResolve struct {
	Target *models.Learning
	// Reason explains why nothing was resolved. Empty when Target is set.
	Reason string
	// Recorded is the supersede reason actually written for Target.
	Recorded string
	// Candidates lists the conflicting open items when the subject names several.
	Candidates []models.Learning
}

// ResolveCommit decides which open task a commit message closes and resolves it.
// It is the only entry point callers should use, so the reason a caller reports
// is the reason that was recorded.
func (s *Store) ResolveCommit(message, project string) (*CommitResolve, error) {
	decision, err := s.ResolveTargetForCommit(message, project)
	if err != nil || decision.Target == nil {
		return decision, err
	}
	reason := commitReason(message)
	if err := s.ResolveLearning(decision.Target.ID, reason); err != nil {
		return nil, fmt.Errorf("resolve #%d by commit: %w", decision.Target.ID, err)
	}
	decision.Recorded = reason
	return decision, nil
}

// commitReason is the supersede reason a commit resolution records.
func commitReason(message string) string {
	return commitReasonPrefix + truncateRunes(strings.TrimSpace(message), commitReasonMaxLen)
}

// ResolveTargetForCommit decides which open task, if any, a commit message
// closes, without resolving it.
func (s *Store) ResolveTargetForCommit(message, project string) (*CommitResolve, error) {
	msg := strings.TrimSpace(message)
	if msg == "" {
		return &CommitResolve{Reason: "empty commit message"}, nil
	}
	if len(msg) > maxCommitMessageBytes {
		return &CommitResolve{Reason: "commit message is too long to match against open tasks"}, nil
	}
	// An unknown project cannot establish scope: guessing "global" would let a
	// commit in one repository close a task in another.
	if project == "" {
		return &CommitResolve{Reason: "no project scope, nothing was matched"}, nil
	}
	subject := commitSubject(msg)
	// git's own revert subject quotes the reverted subject, anchor included.
	// Closing a task because the work was undone is exactly backwards.
	if strings.HasPrefix(subject, `Revert "`) {
		return &CommitResolve{Reason: "commit reverts earlier work, not resolving"}, nil
	}

	target, reason, candidates, err := s.commitAnchorTarget(subject, project, commitTerms(subject))
	if err != nil {
		return nil, err
	}
	return &CommitResolve{Target: target, Reason: reason, Candidates: candidates}, nil
}

// commitAnchorTarget resolves explicit references in the subject. It returns
// (nil, "", nil) when the subject names no open item, (nil, reason, candidates)
// when it names several, and the item when it names exactly one.
func (s *Store) commitAnchorTarget(subject, project string, terms []string) (*models.Learning, string, []models.Learning, error) {
	anchored := make(map[int64]models.Learning)

	ids := make(map[int64]bool)
	for _, m := range learningIDAnchorRe.FindAllStringSubmatch(subject, -1) {
		if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			ids[id] = true
		}
	}
	keys := make(map[string]bool)
	for _, key := range ticketKeyAnchorRe.FindAllString(subject, -1) {
		if !technicalKeyPrefixes[strings.SplitN(key, "-", 2)[0]] {
			keys[key] = true
		}
	}
	// Checked on the distinct references: repeating one is not ambiguity, and
	// silently dropping references would hide a legitimate target.
	if len(ids) > maxAnchorsPerSubject {
		return nil, fmt.Sprintf("commit subject names %d different learning ids, pass the one to close", len(ids)), nil, nil
	}
	if len(keys) > maxAnchorsPerSubject {
		return nil, fmt.Sprintf("commit subject names %d different ticket keys, pass the learning id to close", len(keys)), nil, nil
	}

	type rejectedID struct {
		id     int64
		shared int
	}
	var rejected []rejectedID
	for id := range ids {
		item, err := s.openUnfinishedByID(id, project)
		if err != nil {
			return nil, "", nil, err
		}
		if item == nil {
			continue
		}
		// "#123" is also how this repository references issues and pull requests.
		// A learning reference shares words with the work it closes; a foreign
		// number reference shares none, and one shared word is the accident class
		// that closed #66092. The id alone is not trusted.
		shared := sharedTermCount(item.Content, terms)
		if shared < minAnchorSharedTerms {
			rejected = append(rejected, rejectedID{id: id, shared: shared})
			continue
		}
		anchored[id] = *item
	}

	var conflicts []string
	var conflictCandidates []models.Learning
	for _, key := range sortedKeys(keys) {
		hits, unsure, err := s.openUnfinishedByKey(key, project)
		if err != nil {
			return nil, "", nil, err
		}
		if unsure {
			conflicts = append(conflicts, fmt.Sprintf("ticket key %s matches at least %d open items, uniqueness cannot be proven", key, keyLookupLimit))
			continue
		}
		switch len(hits) {
		case 0:
		case 1:
			anchored[hits[0].ID] = hits[0]
		default:
			// A key carried by several items cannot distinguish them. Recorded, not
			// returned yet: a unique id anchor elsewhere in the subject still decides.
			conflicts = append(conflicts, fmt.Sprintf("ticket key %s names %d open items", key, len(hits)))
			for _, hit := range hits {
				if len(conflictCandidates) == maxCommitCandidates {
					break
				}
				conflictCandidates = append(conflictCandidates, hit)
			}
		}
	}

	switch len(anchored) {
	case 0:
		if len(conflicts) > 0 {
			return nil, strings.Join(conflicts, "; ") + ", pass the learning id to close", conflictCandidates, nil
		}
		if len(rejected) > 0 {
			r := rejected[0]
			if len(terms) < minAnchorSharedTerms {
				return nil, fmt.Sprintf("learning #%d is open but the commit subject has fewer than %d distinct words to verify it; use resolve(learning_id=%d) instead",
					r.id, minAnchorSharedTerms, r.id), nil, nil
			}
			return nil, fmt.Sprintf("learning #%d is open but shares only %d of the %d subject words an anchor needs; use resolve(learning_id=%d) instead",
				r.id, r.shared, minAnchorSharedTerms, r.id), nil, nil
		}
		return nil, "", nil, nil
	case 1:
		for _, item := range anchored {
			target := item
			return &target, "", nil, nil
		}
	}

	candidates := make([]models.Learning, 0, len(anchored)+len(conflictCandidates))
	candidates = append(candidates, conflictCandidates...)
	for _, id := range sortedLearningIDs(anchored) {
		candidates = append(candidates, anchored[id])
	}
	if len(candidates) > maxCommitCandidates {
		candidates = candidates[:maxCommitCandidates]
	}
	return nil, fmt.Sprintf("commit names %d different open items, pass a single learning id", len(anchored)), candidates, nil
}

// openUnfinishedByID fetches one open task by id. Callers guarantee a non-empty
// project: an unknown scope must not resolve anything, so it is refused before
// the lookup.
func (s *Store) openUnfinishedByID(id int64, project string) (*models.Learning, error) {
	query := `SELECT ` + unfinishedSelectColumns + ` FROM learnings l` + s.openUnfinishedWhere(project) + ` AND l.id = ?`
	args := append(s.openUnfinishedArgs(project), id)
	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("open unfinished %d: %w", id, err)
	}
	defer rows.Close()
	found, err := scanLearnings(rows)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	return &found[0], nil
}

// openUnfinishedByKey finds the open tasks whose content carries key as a
// standalone token. The SQL prefilter runs in the database, so the item does not
// have to be inside any candidate window.
func (s *Store) openUnfinishedByKey(key, project string) ([]models.Learning, bool, error) {
	query := `SELECT ` + unfinishedSelectColumns + ` FROM learnings l` + s.openUnfinishedWhere(project) +
		fmt.Sprintf(` AND instr(upper(l.content), ?) > 0 ORDER BY l.id DESC LIMIT %d`, keyLookupLimit)
	args := append(s.openUnfinishedArgs(project), strings.ToUpper(key))
	rows, err := s.readerDB().Query(query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("open unfinished by key %s: %w", key, err)
	}
	defer rows.Close()
	prefiltered, err := scanLearnings(rows)
	if err != nil {
		return nil, false, err
	}
	// A full window cannot prove uniqueness: the token filter may drop rows outside
	// it, so a second carrier could stay invisible. An anchor needs certainty.
	if len(prefiltered) >= keyLookupLimit {
		return nil, true, nil
	}
	hits := prefiltered[:0]
	for _, item := range prefiltered {
		if containsTokenFold(item.Content, key) {
			hits = append(hits, item)
		}
	}
	return hits, false, nil
}

// openUnfinishedWhere is the predicate shared by every open-task lookup.
func (s *Store) openUnfinishedWhere(project string) string {
	// Strict equality on purpose: the `OR canonical_project = ''` tolerance of the
	// search path is a discovery convenience, but a resolve decision must not
	// close a row that belongs to no project from inside any project.
	return ` WHERE ` + models.RecallableLearningSQLAlias + ` AND l.category = 'unfinished'
		AND l.canonical_project = ?`
}

func (s *Store) openUnfinishedArgs(project string) []any {
	return []any{s.resolveCanonicalProject(project)}
}

// sortedIDs returns the ids in ascending order so the reported reason does not
// depend on map iteration order.
func sortedIDs(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func sortedLearningIDs(set map[int64]models.Learning) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sharedTermCount counts how many of the terms text contains.
func sharedTermCount(text string, terms []string) int {
	tokens := tokenSet(text)
	shared := 0
	for _, term := range terms {
		if tokens[term] {
			shared++
		}
	}
	return shared
}

// commitSubject is the first non-empty line: the body is prose, and a ticket
// mentioned there must not outrank the subject.
func commitSubject(message string) string {
	for _, line := range strings.Split(message, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// commitTerms returns the distinct significant words of a commit subject. It
// only feeds the id-anchor guard, so filler words and bare numbers are dropped:
// they are shared with far too many items to prove anything.
func commitTerms(subject string) []string {
	var terms []string
	seen := make(map[string]bool)
	for _, token := range tokenCores(subject) {
		if len(token) < 2 || commitStopWords[token] || seen[token] || isDigitsOnly(token) {
			continue
		}
		seen[token] = true
		terms = append(terms, token)
	}
	return terms
}

// isDigitsOnly reports whether a token consists of digits only.
func isDigitsOnly(token string) bool {
	for _, r := range token {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(token) > 0
}

// tokenCores splits text into its alphanumeric cores, lowercased, the same way
// the FTS5 tokenizer does: hyphens and punctuation separate words.
func tokenCores(text string) []string {
	var cores []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			cores = append(cores, current.String())
			current.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return cores
}

// tokenSet returns the set of alphanumeric cores in text.
func tokenSet(text string) map[string]bool {
	tokens := make(map[string]bool)
	for _, core := range tokenCores(text) {
		tokens[core] = true
	}
	return tokens
}

// containsTokenFold reports whether text contains key (already upper case) as a
// standalone token: "YM-26" must not report a hit on "YM-260".
func containsTokenFold(text, key string) bool {
	upper := strings.ToUpper(text)
	for offset := 0; offset < len(upper); {
		idx := strings.Index(upper[offset:], key)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(key)
		if !alnumBefore(upper, start) && !alnumAfter(upper, end) {
			return true
		}
		offset = start + 1
	}
	return false
}

// alnumBefore reports whether the rune ending before byte offset is alphanumeric.
func alnumBefore(s string, offset int) bool {
	if offset <= 0 || offset > len(s) {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:offset])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// alnumAfter reports whether the rune starting at byte offset is alphanumeric.
func alnumAfter(s string, offset int) bool {
	if offset >= len(s) || offset < 0 {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[offset:])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// truncateRunes shortens text to at most maxBytes without splitting a UTF-8
// character; a lone continuation byte in supersede_reason would corrupt the
// audit trail.
func truncateRunes(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// commitStopWords are function words plus the conventional-commit vocabulary.
// They only feed the id-anchor guard: a learning reference must share a
// substantive word with the item it closes.
var commitStopWords = map[string]bool{
	// English function words.
	"a": true, "an": true, "and": true, "the": true, "to": true, "of": true, "for": true,
	"in": true, "on": true, "at": true, "by": true, "with": true, "from": true, "into": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true, "it": true,
	"its": true, "this": true, "that": true, "these": true, "those": true, "but": true,
	"or": true, "not": true, "no": true, "as": true, "if": true, "then": true, "than": true,
	"so": true, "we": true, "you": true, "they": true, "them": true, "our": true, "your": true,
	"their": true, "my": true, "me": true, "us": true, "do": true, "does": true, "did": true,
	"done": true, "can": true, "could": true, "should": true, "would": true, "will": true,
	"shall": true, "may": true, "might": true, "must": true, "have": true, "has": true,
	"had": true, "also": true, "only": true, "just": true, "more": true, "most": true,
	"less": true, "very": true, "all": true, "any": true, "some": true, "each": true,
	"per": true, "via": true, "after": true, "before": true, "when": true, "while": true,
	"which": true, "who": true, "what": true, "how": true, "why": true, "where": true,
	// Conventional commit types and generic verbs.
	"fix": true, "fixes": true, "fixed": true, "feat": true, "feature": true, "features": true,
	"chore": true, "docs": true, "doc": true, "test": true, "tests": true, "testing": true,
	"refactor": true, "refactoring": true, "style": true, "perf": true, "build": true,
	"ci": true, "revert": true, "wip": true, "merge": true, "bump": true, "add": true,
	"adds": true, "added": true, "update": true, "updates": true, "updated": true,
	"remove": true, "removes": true, "removed": true, "use": true, "uses": true, "used": true,
	"make": true, "makes": true, "made": true, "move": true, "moves": true, "moved": true,
	"rename": true, "renamed": true, "change": true, "changes": true, "changed": true,
	"set": true, "sets": true, "support": true, "supports": true, "improve": true,
	"improves": true, "implement": true, "implements": true, "introduce": true, "enable": true,
	"enables": true, "disable": true, "disables": true, "allow": true, "allows": true,
	"keep": true, "keeps": true, "avoid": true, "prevent": true, "prevents": true,
	"handle": true, "handles": true, "commit": true, "branch": true, "main": true,
	"yesloop": true, "los": true,
	// German function words.
	"der": true, "die": true, "das": true, "den": true, "dem": true, "des": true,
	"und": true, "oder": true, "aber": true, "nicht": true, "kein": true, "keine": true,
	"ist": true, "sind": true, "war": true, "waren": true, "wird": true, "werden": true,
	"wurde": true, "wurden": true, "hat": true, "haben": true, "hatte": true, "hatten": true,
	"ein": true, "eine": true, "einen": true, "einem": true, "einer": true, "eines": true,
	"zu": true, "zum": true, "zur": true, "von": true, "vom": true, "mit": true,
	"für": true, "fuer": true, "auf": true, "aus": true, "bei": true, "nach": true,
	"vor": true, "über": true, "ueber": true, "unter": true, "im": true, "am": true,
	"als": true, "auch": true, "noch": true, "nur": true, "sich": true,
	"sie": true, "wir": true, "ich": true, "du": true, "er": true, "dass": true,
	"wie": true, "wer": true, "wo": true, "wenn": true, "dann": true, "sowie": true,
	"bzw": true, "etc": true,
}
