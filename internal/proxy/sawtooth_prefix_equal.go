package proxy

import "math"

// los26eJSONValuesEqual reports whether two values decoded from JSON are
// structurally equal. The snapshot compares the stored prefix against the one
// the request carries on every turn; marshalling both sides and hashing them
// answers the same question at the cost of several megabytes of allocation per
// request.
//
// Only the types encoding/json produces are handled, and floats compare by
// their bits: -0 and 0 are equal in Go but marshal to different bytes, so a
// value comparison would call a prefix unchanged that did change on the wire.
// Anything else counts as different so the caller falls back to the byte
// comparison instead of claiming equality it cannot prove.
func los26eJSONValuesEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			w, ok := bv[k]
			if !ok || !los26eJSONValuesEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !los26eJSONValuesEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && math.Float64bits(av) == math.Float64bits(bv)
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	}
	return false
}

// los26eFrozenPrefixEqual is the typed entry point. Reaching the recursive
// comparison directly with both slices would box two slice headers on every
// request.
func los26eFrozenPrefixEqual(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !los26eJSONValuesEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}
