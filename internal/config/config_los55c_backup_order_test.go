package config

import "testing"

// Cold review nit 9: within one second the counter suffix is the only ordering
// signal, and "-10" sorts before "-2" lexicographically. The prune then deleted
// the newer copies and kept the older ones.
func TestLos55c_BackupOrderComparesTheCounterNumerically(t *testing.T) {
	older := backupEntry{name: "config.yaml.bak.20260101T000000-2"}
	newer := backupEntry{name: "config.yaml.bak.20260101T000000-10"}

	if !backupOrderLess(older, newer) {
		t.Error("backupOrderLess put ...-10 before ...-2: the counter must order numerically")
	}
	if backupOrderLess(newer, older) {
		t.Error("backupOrderLess is not antisymmetric for the counter suffix")
	}
	if !backupOrderLess(backupEntry{name: "config.yaml.bak.20260101T000000"}, older) {
		t.Error("the unsuffixed backup is the first of its second")
	}
}
