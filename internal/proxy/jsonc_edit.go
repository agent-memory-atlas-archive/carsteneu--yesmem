package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// This file holds the comment- and order-preserving edit path for opencode's
// config. Why not unmarshal + marshal: Go sorts map keys and drops comments, so
// a config the user maintains by hand was rewritten top to bottom on every proxy
// start (YM-145). Everything here works on byte offsets instead and leaves every
// byte it does not need to change untouched.

// blankJSONCNoise returns a copy of src where every comment byte and every
// trailing comma is replaced by a space. Newlines survive and the length is
// unchanged, so byte offsets in the result are byte offsets in src.
// encoding/json can then parse the document; the returned slice must never be
// written back to the file.
func blankJSONCNoise(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	// A UTF-8 BOM is not whitespace for encoding/json. Blanking it keeps offsets.
	if len(out) >= 3 && out[0] == 0xEF && out[1] == 0xBB && out[2] == 0xBF {
		out[0], out[1], out[2] = ' ', ' ', ' '
	}
	blankJSONComments(out)
	blankTrailingCommas(out)
	return out
}

// blankJSONComments blanks // and /* */ comments IN PLACE, leaving string
// literals alone. Callers must pass a scratch copy: on the original document
// this would destroy the comments before the file is written back.
func blankJSONComments(out []byte) []byte {
	inString := false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if inString {
			if c == '\\' && i+1 < len(out) {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '/':
			if i+1 >= len(out) {
				continue
			}
			switch out[i+1] {
			case '/':
				for i < len(out) && out[i] != '\n' {
					out[i] = ' '
					i++
				}
			case '*':
				out[i], out[i+1] = ' ', ' '
				i += 2
				for i < len(out) {
					if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
						out[i], out[i+1] = ' ', ' '
						i++
						break
					}
					if out[i] != '\n' {
						out[i] = ' '
					}
					i++
				}
			}
		}
	}
	return out
}

// blankTrailingCommas removes the commas encoding/json rejects but JSONC allows,
// i.e. a comma whose next non-space byte closes a container.
func blankTrailingCommas(out []byte) {
	inString := false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if inString {
			if c == '\\' && i+1 < len(out) {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		if c != ',' {
			continue
		}
		j := i + 1
		for j < len(out) && isJSONSpace(out[j]) {
			j++
		}
		if j < len(out) && (out[j] == '}' || out[j] == ']') {
			out[i] = ' '
		}
	}
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// jsonTopLevelDuplicateKey reports the first key that appears more than once in
// the top-level object, or "". Duplicate keys have undefined precedence across
// parsers, so a structural patch cannot know which block is the effective one.
func jsonTopLevelDuplicateKey(doc []byte) string {
	dec := json.NewDecoder(bytes.NewReader(blankJSONCNoise(doc)))
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	if d, isDelim := tok.(json.Delim); !isDelim || d != '{' {
		return ""
	}
	seen := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return ""
		}
		name, isString := keyTok.(string)
		if !isString {
			return ""
		}
		if seen[name] {
			return name
		}
		seen[name] = true
		valTok, err := dec.Token()
		if err != nil {
			return ""
		}
		if d, isDelim := valTok.(json.Delim); isDelim {
			closer := '}'
			if d == '[' {
				closer = ']'
			}
			if _, err := skipContainer(dec, d, json.Delim(closer)); err != nil {
				return ""
			}
		}
	}
	return ""
}

// jsoncObjectSpan returns the half-open byte span of the object value at path.
// The path walks nested keys from the document root, e.g.
// ("provider", "deepseek", "options"). ok is false when any step is missing or
// is not an object.
func jsoncObjectSpan(doc []byte, path ...string) (start, end int, ok bool) {
	if len(path) == 0 {
		return 0, 0, false
	}
	dec := json.NewDecoder(bytes.NewReader(blankJSONCNoise(doc)))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false
	}
	if d, isDelim := tok.(json.Delim); !isDelim || d != '{' {
		return 0, 0, false
	}
	return spanInObject(dec, path)
}

// spanInObject walks one object and returns the span of the object at path.
// On return the decoder sits behind that object.
func spanInObject(dec *json.Decoder, path []string) (int, int, bool) {
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		key, isString := keyTok.(string)
		if !isString {
			return 0, 0, false
		}
		valTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		open, isDelim := valTok.(json.Delim)
		if isDelim && open == '{' {
			// InputOffset points behind the '{' we just read.
			objStart := int(dec.InputOffset()) - 1
			if key == path[0] {
				if len(path) == 1 {
					objEnd, err := skipContainer(dec, '{', '}')
					if err != nil {
						return 0, 0, false
					}
					return objStart, objEnd, true
				}
				if s, e, found := spanInObject(dec, path[1:]); found {
					return s, e, true
				}
				continue // recursion consumed the object
			}
			if _, err := skipContainer(dec, '{', '}'); err != nil {
				return 0, 0, false
			}
			continue
		}
		if isDelim && open == '[' {
			if _, err := skipContainer(dec, '[', ']'); err != nil {
				return 0, 0, false
			}
		}
		// scalars are already consumed by the Token call above
	}
	return 0, 0, false
}

// jsonTopLevelStringSpan returns the byte span of a string value stored under key
// at the TOP level of the document. Quotes are excluded: start is the opening
// quote, end the closing quote. Locating the field structurally matters: a
// bytes.Index over the whole document would hit a nested key of the same name
// first and rewrite the wrong field.
func jsonTopLevelStringSpan(doc []byte, key string) (start, end int, ok bool) {
	// Scan the noise-free copy: offsets are identical (blanking keeps the length),
	// and a comment between key and colon then reads as whitespace.
	scan := blankJSONCNoise(doc)
	dec := json.NewDecoder(bytes.NewReader(scan))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false
	}
	if d, isDelim := tok.(json.Delim); !isDelim || d != '{' {
		return 0, 0, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		name, isString := keyTok.(string)
		if !isString {
			return 0, 0, false
		}
		afterKey := int(dec.InputOffset())
		valStart, ok := valueStartAfterKey(scan, afterKey)
		if !ok {
			return 0, 0, false
		}
		if name == key {
			if valStart >= len(scan) || scan[valStart] != '"' {
				return 0, 0, false
			}
			valEnd := scanJSONStringEnd(scan, valStart)
			if valEnd < 0 {
				return 0, 0, false
			}
			return valStart, valEnd, true
		}
		valTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		if d, isDelim := valTok.(json.Delim); isDelim {
			closer := '}'
			if d == '[' {
				closer = ']'
			}
			if _, err := skipContainer(dec, d, json.Delim(closer)); err != nil {
				return 0, 0, false
			}
		}
	}
	return 0, 0, false
}

// valueStartAfterKey skips the colon and whitespace behind a consumed key token
// and returns the offset of the value's first byte.
func valueStartAfterKey(doc []byte, afterKey int) (int, bool) {
	i := afterKey
	for i < len(doc) && isJSONSpace(doc[i]) {
		i++
	}
	if i >= len(doc) || doc[i] != ':' {
		return 0, false
	}
	i++
	for i < len(doc) && isJSONSpace(doc[i]) {
		i++
	}
	if i >= len(doc) {
		return 0, false
	}
	return i, true
}

// scanJSONStringEnd returns the offset of the closing quote of the string that
// starts at start, or -1.
func scanJSONStringEnd(doc []byte, start int) int {
	if start >= len(doc) || doc[start] != '"' {
		return -1
	}
	for i := start + 1; i < len(doc); i++ {
		switch doc[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// replaceJSONStringValue rewrites the string value of a TOP-level JSON field in
// place, without re-serializing the document. Callers need this where key order
// and byte positions must survive (fork bodies share the main request's cache
// prefix). Returns doc unchanged when the field is absent or is not a string.
func replaceJSONStringValue(doc []byte, key, value string) []byte {
	start, end, ok := jsonTopLevelStringSpan(doc, key)
	if !ok {
		return doc
	}
	out := make([]byte, 0, len(doc)+len(value))
	out = append(out, doc[:start+1]...)
	out = append(out, value...)
	out = append(out, doc[end:]...)
	return out
}

// jsonTopLevelValueSpan returns the byte span of the value stored under key at the
// TOP level of the document, quotes of a string included. It locates the field
// structurally: a byte search would hit a sibling that carries the same literal
// first and rewrite the wrong place (YM-262).
func jsonTopLevelValueSpan(doc []byte, key string) (start, end int, ok bool) {
	scan := blankJSONCNoise(doc)
	dec := json.NewDecoder(bytes.NewReader(scan))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false
	}
	if d, isDelim := tok.(json.Delim); !isDelim || d != '{' {
		return 0, 0, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		name, isString := keyTok.(string)
		if !isString {
			return 0, 0, false
		}
		afterKey := int(dec.InputOffset())
		valStart, ok := valueStartAfterKey(scan, afterKey)
		if !ok {
			return 0, 0, false
		}
		valTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		valEnd := int(dec.InputOffset())
		if d, isDelim := valTok.(json.Delim); isDelim {
			closer := json.Delim('}')
			if d == '[' {
				closer = ']'
			}
			valEnd, err = skipContainer(dec, d, closer)
			if err != nil {
				return 0, 0, false
			}
		}
		// InputOffset points behind the token; a scalar keeps its trailing
		// whitespace in the span otherwise.
		for valEnd > valStart && isJSONSpace(scan[valEnd-1]) {
			valEnd--
		}
		if name == key {
			return valStart, valEnd, true
		}
	}
	return 0, 0, false
}

// replaceTopLevelJSONValue replaces the value of a TOP-level field in place and
// leaves every other byte of the document alone, so a fork body keeps the main
// request's cache prefix. ok is false when the field is absent.
func replaceTopLevelJSONValue(doc []byte, key string, value []byte) ([]byte, bool) {
	start, end, ok := jsonTopLevelValueSpan(doc, key)
	if !ok {
		return nil, false
	}
	out := make([]byte, 0, len(doc)-(end-start)+len(value))
	out = append(out, doc[:start]...)
	out = append(out, value...)
	out = append(out, doc[end:]...)
	return out, true
}

// skipContainer consumes the remainder of the container the decoder is inside and
// returns the byte offset behind its closing delimiter.
func skipContainer(dec *json.Decoder, open, close json.Delim) (int, error) {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return 0, err
		}
		d, isDelim := tok.(json.Delim)
		if !isDelim {
			continue
		}
		switch d {
		case open:
			depth++
		case close:
			depth--
		}
	}
	return int(dec.InputOffset()), nil
}

// insertJSONCObjectField inserts field as the first member of the object at path
// and returns the edited document. Indentation and line ending follow the
// document's own formatting; a single-line object gets an inline member. ok is
// false when the path does not resolve to an object.
func insertJSONCObjectField(doc []byte, field string, path ...string) ([]byte, bool) {
	start, end, ok := jsoncObjectSpan(doc, path...)
	if !ok || end <= start+1 {
		return nil, false
	}
	eol := "\n"
	if nl := bytes.LastIndexByte(doc[:start], '\n'); nl > 0 && doc[nl-1] == '\r' {
		eol = "\r\n"
	}
	inner := doc[start+1 : end-1]
	var insert string
	switch {
	case len(bytes.TrimSpace(inner)) == 0:
		insert = field
	case !bytes.ContainsRune(inner, '\n'):
		insert = field + ", "
	default:
		// Reuse the indentation of the object's first member line.
		nl := bytes.IndexByte(doc[start:], '\n')
		if nl < 0 {
			return nil, false
		}
		lineStart := start + nl + 1
		lineEnd := lineStart
		for lineEnd < len(doc) && (doc[lineEnd] == ' ' || doc[lineEnd] == '\t') {
			lineEnd++
		}
		indent := string(doc[lineStart:lineEnd])
		if indent == "" {
			indent = "  "
		}
		insert = eol + indent + field + ","
	}
	out := make([]byte, 0, len(doc)+len(insert))
	out = append(out, doc[:start+1]...)
	out = append(out, insert...)
	out = append(out, doc[start+1:]...)
	return out, true
}

// patchOpenCodeBaseURLs sets options.baseURL to the local proxy for each provider
// in providerIDs that has no baseURL yet. It returns the edited document and the
// number of providers actually patched. bytes is nil when nothing changed; the
// caller must then leave the file alone.
//
// A single unusable provider block (wrong type, unparseable) does not cancel the
// patch for the others: each block is decoded on its own and skipped on error.
// Before anything is written, the result is parsed again and every patched
// provider must show up with a baseURL; otherwise the caller keeps the old file.
// optionsState classifies a provider block's options field. exists is false when
// the block has no options at all (then a new options object may be inserted).
// isObject is false when options is present but not an object (null, string,
// array): inserting a second "options" key there would produce a duplicate and
// a dead patch, so the caller skips that provider instead.
//
// The block is decoded as a generic map on purpose: decoding into a struct with
// a map field merges duplicated "options" objects instead of replacing them,
// which hides the effective block from the check.
func optionsState(raw json.RawMessage) (exists, isObject, hasBaseURL bool, parseable bool) {
	var block map[string]any
	if err := json.Unmarshal(raw, &block); err != nil {
		return false, false, false, false
	}
	value, present := block["options"]
	if !present {
		return false, false, false, true
	}
	options, stillObject := value.(map[string]any)
	if !stillObject {
		return true, false, false, true
	}
	_, hasBaseURL = options["baseURL"]
	return true, true, hasBaseURL, true
}

// patchOpenCodeBaseURLs sets options.baseURL to the local proxy for each provider
// in providerIDs that has no baseURL yet. It returns the edited document and the
// number of providers actually patched. The document is nil when nothing changed;
// the caller must then leave the file alone.
//
// A single unusable provider block (wrong type, non-object options, unparseable)
// does not cancel the patch for the others: each block is decoded on its own and
// skipped on error. Before anything is written, the result is parsed again and
// every patched provider must show up with a baseURL; otherwise no document is
// returned at all.
func patchOpenCodeBaseURLs(data []byte, providerIDs []string) ([]byte, int, error) {
	if dup := jsonTopLevelDuplicateKey(data); dup != "" {
		return nil, 0, fmt.Errorf("config has duplicate top-level key %q; refusing to patch", dup)
	}
	var top struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(blankJSONCNoise(data), &top); err != nil {
		return nil, 0, fmt.Errorf("parse config: %w", err)
	}
	if top.Provider == nil {
		return nil, 0, nil
	}

	const proxyBaseURL = "http://localhost:9099/v1"
	field := `"baseURL": "` + proxyBaseURL + `"`

	out := data
	var patchedIDs []string
	for _, providerID := range providerIDs {
		raw, exists := top.Provider[providerID]
		if !exists {
			continue
		}
		hasOptions, optionsIsObject, hasBaseURL, parseable := optionsState(raw)
		if !parseable {
			continue // unusable block: leave it untouched
		}
		if hasBaseURL {
			continue
		}
		if hasOptions && !optionsIsObject {
			continue // cannot extend a non-object options value without duplicating the key
		}
		var edited []byte
		var ok bool
		if hasOptions {
			edited, ok = insertJSONCObjectField(out, field, "provider", providerID, "options")
		} else {
			edited, ok = insertJSONCObjectField(out, `"options": {`+field+`}`, "provider", providerID)
		}
		if !ok {
			continue
		}
		out = edited
		patchedIDs = append(patchedIDs, providerID)
	}

	if len(patchedIDs) == 0 {
		return nil, 0, nil
	}

	var verify struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(blankJSONCNoise(out), &verify); err != nil {
		return nil, 0, fmt.Errorf("patched config does not parse: %w", err)
	}
	for _, providerID := range patchedIDs {
		raw, exists := verify.Provider[providerID]
		if !exists {
			return nil, 0, fmt.Errorf("patched config lost provider %q", providerID)
		}
		_, _, hasBaseURL, parseable := optionsState(raw)
		if !parseable {
			return nil, 0, fmt.Errorf("patched provider %q is unreadable", providerID)
		}
		if !hasBaseURL {
			return nil, 0, fmt.Errorf("patch did not reach the effective options of provider %q (duplicate keys?)", providerID)
		}
	}
	return out, len(patchedIDs), nil
}

// writeFileAtomic replaces path with data through a temp file in the same
// directory and a rename. A crash mid-write must not truncate the user's config,
// and the rename keeps the configured mode instead of inheriting the umask.
//
// A symlinked config (dotfiles, stow, chezmoi) is written through to its target:
// renaming over the link would silently replace the link with a regular file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return writeFileAtomicPolicy(path, data, mode, true)
}

// writeFileAtomicNoFollow is writeFileAtomic for yesmem's own artifacts. A
// symlink at path is replaced by the regular file instead of being resolved and
// written through: the rename never follows a link, so nothing that a link at
// that path points at can be overwritten, and no decision on this path redirects
// the content.
func writeFileAtomicNoFollow(path string, data []byte, mode os.FileMode) error {
	return writeFileAtomicPolicy(path, data, mode, false)
}

func writeFileAtomicPolicy(path string, data []byte, mode os.FileMode, followSymlink bool) error {
	if followSymlink {
		// Resolve a symlinked path before the rename. Renaming over the link would
		// replace the link itself with a regular file, which silently detaches
		// dotfile-managed configs. An unresolvable link is an error, not a fallback:
		// falling back would do exactly the damage this check prevents.
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			resolved, rerr := filepath.EvalSymlinks(path)
			if rerr != nil {
				return fmt.Errorf("cannot resolve symlink %s: %w", path, rerr)
			}
			path = resolved
		}
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".opencode-patch-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// The mode is applied after the payload. Chmod'ing first widens the temp file
	// to the target mode while it is still being written, which exposes a
	// half-written config to every reader the target mode allows.
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // renamed, nothing to clean up
	// Persist the rename itself: without the directory sync a crash can leave
	// the old file, or no file, in place of the one just written.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
