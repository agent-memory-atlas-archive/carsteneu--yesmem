package storage

import (
	"testing"
	"time"
)

// YM-295 (2): the fuzzy fallback interpolated the caller's value into a LIKE
// pattern. "_" and "%" are wildcards there, so "pro_ect" matched a project called
// "proXect" and routed the caller into a checkout it never named.
func TestLos32gResolveProjectShortEscapesLikeMetacharacters(t *testing.T) {
	s := mustOpenFiles(t)
	now := time.Now()
	insertSessionForResolve(t, s, "sess-los32g-1", "/srv/proXect", "/srv/proXect", now)
	insertSessionForResolve(t, s, "sess-los32g-2", "/srv/a/b", "/srv/a/b", now)

	if got := s.ResolveProjectShort("pro_ect"); got != "pro_ect" {
		t.Errorf("ResolveProjectShort(%q) = %q, want the literal name back", "pro_ect", got)
	}
	if got := s.ResolveProjectShort("a%b"); got != "a%b" {
		t.Errorf("ResolveProjectShort(%q) = %q, want the literal name back", "a%b", got)
	}
	// A name that really occurs still resolves through the fallback.
	if got := s.ResolveProjectShort("proXect"); got != "/srv/proXect" {
		t.Errorf("ResolveProjectShort(%q) = %q, want the matching project", "proXect", got)
	}
}
