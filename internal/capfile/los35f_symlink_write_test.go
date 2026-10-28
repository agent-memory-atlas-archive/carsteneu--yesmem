package capfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// YM-359 (OPS-88, key 3): WriteFile wrote the rendered cap to "<dir>/CAP.md.tmp"
// with os.WriteFile, which follows a symlink planted at that path. The cap body
// then landed in whatever file the link named, and the rename afterwards moved
// the link into place as CAP.md.
func TestLos35fWriteFileDoesNotWriteThroughASymlinkedTempPath(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "CAP.md.tmp")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	err := WriteFile(los35eProbeCap(), dir)
	if err == nil {
		t.Fatal("WriteFile accepted a symlinked temp path")
	}

	data, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatalf("read victim: %v", readErr)
	}
	if string(data) != "untouched" {
		t.Fatalf("the cap body was written through the symlink, victim = %q", data)
	}
	if info, err := os.Lstat(filepath.Join(dir, "CAP.md")); err == nil && info.Mode()&os.ModeSymlink != 0 {
		t.Error("CAP.md became a symlink to the victim")
	}
}

// A symlink at CAP.md itself has to be refused as well. The rename would replace
// the link rather than follow it, but the file it points at is still the wrong
// destination for a cap body.
func TestLos35fWriteFileRefusesASymlinkedTarget(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	target := filepath.Join(dir, "CAP.md")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if err := WriteFile(los35eProbeCap(), dir); err == nil {
		t.Fatal("WriteFile accepted a symlinked CAP.md")
	}

	data, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatalf("read victim: %v", readErr)
	}
	if string(data) != "untouched" {
		t.Fatalf("the cap body reached the symlink target, victim = %q", data)
	}
}

// The repair the export branch runs must not follow a link either, and it must
// still do its job on a regular file.
func TestLos35fRepairFileOwnerOnlyRefusesASymlink(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	link := filepath.Join(t.TempDir(), "CAP.md")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if err := RepairFileOwnerOnly(link); err == nil {
		t.Fatal("RepairFileOwnerOnly followed a symlink")
	}
	info, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("stat victim: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("the mode repair reached the symlink target, mode = %04o", perm)
	}
}

func TestLos35fRepairFileOwnerOnlyRepairsTheMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CAP.md")
	if err := os.WriteFile(path, []byte("---\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := RepairFileOwnerOnly(path); err != nil {
		t.Fatalf("repair: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %04o, want 0600", perm)
	}
}

// A regular leftover from a crashed write must not block the next export; only a
// link at the temp path does.
func TestLos35fWriteOwnerOnlyReplacesACrashedTempFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CAP.md")
	if err := os.WriteFile(path+".tmp", []byte("half a cap"), 0o600); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	if err := WriteOwnerOnly(path, []byte("---\nname: los35f\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "---\nname: los35f\n" {
		t.Fatalf("content = %q", data)
	}
}

// fchmod needs an open descriptor, not a readable one: a write-only cap file from
// a botched repair is still fixable.
func TestLos35fRepairFileOwnerOnlyHandlesAWriteOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CAP.md")
	if err := os.WriteFile(path, []byte("---\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatalf("chmod write-only: %v", err)
	}

	if err := RepairFileOwnerOnly(path); err != nil {
		t.Fatalf("repair: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %04o, want 0600", perm)
	}
}

// The export reads CAP.md before it compares versions. That read must not follow a
// link either.
func TestLos35fReadNoFollowRefusesASymlink(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("---\n"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	link := filepath.Join(t.TempDir(), "CAP.md")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if _, err := ReadNoFollow(link); err == nil {
		t.Fatal("ReadNoFollow followed a symlink")
	}
}

// A FIFO is not a cap file, and open(2) blocks on one, so the guard has to run
// before the open: otherwise the export and the installer hang on a path they
// should simply refuse (YM-359).
func TestLos35fHelpersRefuseAFifoWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CAP.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- RepairFileOwnerOnly(path) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RepairFileOwnerOnly accepted a FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RepairFileOwnerOnly blocked on a FIFO")
	}

	readDone := make(chan error, 1)
	go func() { _, err := ReadNoFollow(path); readDone <- err }()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("ReadNoFollow accepted a FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadNoFollow blocked on a FIFO")
	}
}

// A cap file is a document of a few kilobytes, so the read is bounded: a planted
// file must not turn the export or the install into an allocation of any size.
func TestLos35fReadNoFollowRefusesAnOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CAP.md")
	if err := os.WriteFile(path, make([]byte, maxCapFileSize+1), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := ReadNoFollow(path); err == nil {
		t.Fatal("ReadNoFollow read a file above the size limit")
	}
}

// A hard link is a regular file, so O_NOFOLLOW does not stop it. The mode change
// would narrow an inode that the cap file shares with whatever else links to it.
func TestLos35fRepairFileOwnerOnlyRefusesAHardLink(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("---\n"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	link := filepath.Join(t.TempDir(), "CAP.md")
	if err := os.Link(victim, link); err != nil {
		t.Fatalf("link: %v", err)
	}

	if err := RepairFileOwnerOnly(link); err == nil {
		t.Fatal("RepairFileOwnerOnly narrowed a hard-linked file")
	}
	info, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("stat victim: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("the mode repair reached the shared inode, mode = %04o", perm)
	}
}
