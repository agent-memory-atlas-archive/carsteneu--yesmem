package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// YM-392 [CFG-04] Teil 1 (Los 56i): MigrateConfig und MoveMisplacedProxyKeys
// schrieben mit os.WriteFile(path, content, 0644). Damit galt fuer beide
// Schreibpfade nicht, was fuer jeden anderen config-Schreiber schon gilt
// (writeConfigFile): atomar ersetzen, Modus 0600, einem praeparierten Symlink
// nicht folgen. config.yaml traegt die API-Keys.
func TestLos56i_MigrateConfigWritesOwnerOnlyAndAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "proxy:\n  enabled: true\n"
	if err := os.WriteFile(path, []byte(body), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}

	if _, err := MigrateConfig(path); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("config.yaml mode = %04o after the migration, want 0600 (the file carries API keys)", perm)
	}
}

// Ein Symlink an config.yaml ist Dotfile-Verwaltung: der atomare Schreiber
// ersetzt den Link durch eine regulaere Datei und laesst das Ziel unberuehrt.
// os.WriteFile schreibt stattdessen DURCH den Link und veraendert das Ziel.
func TestLos56i_MigrateConfigDoesNotWriteThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles-config.yaml")
	const original = "proxy:\n  enabled: true\n"
	if err := os.WriteFile(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := MigrateConfig(path); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Error("the migration wrote through the symlink and changed the dotfile target")
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Error("config.yaml is still a symlink after the migration, want the path replaced by the new file")
	}
}

// YM-392 Teil 2: SetValue ist ungesichertes Read-Modify-Write. Zwei Schreiber
// (CLI "yesmem config set" und der Daemon via set_config) lesen denselben Stand
// und der zweite ueberschreibt die Aenderung des ersten. Der Test haelt den
// Schreib-Lock selbst und belegt, dass SetValue wartet, statt daneben zu
// schreiben.
func TestLos56i_SetValueWaitsForTheConfigWriteLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("proxy:\n  enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}

	release, err := holdConfigWriteLock(path)
	if err != nil {
		t.Fatalf("take the write lock: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- SetValue(path, "proxy.reset_cache", true) }()

	select {
	case err := <-done:
		release()
		t.Fatalf("SetValue wrote while another writer held the config lock (err=%v): the read-modify-write is unguarded and loses updates", err)
	case <-time.After(300 * time.Millisecond):
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SetValue after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SetValue did not proceed after the lock was released")
	}

	value, err := GetValue(path, "proxy.reset_cache")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if value != true {
		t.Errorf("proxy.reset_cache = %v, want true", value)
	}
}

// holdConfigWriteLock nimmt denselben Lock, den die Schreibpfade nehmen, und
// gibt die Freigabe zurueck. Der Test steht damit an der Stelle eines zweiten
// Schreibers, ohne einen zweiten Prozess zu brauchen.
func holdConfigWriteLock(path string) (func(), error) {
	// The lock file lives next to config.yaml; the name is the contract between
	// the writers and this test.
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		})
	}, nil
}

// Zwei parallele SetValue auf verschiedene Keys duerfen sich nicht verlieren.
// Ohne Lock ist der Verlust wahrscheinlich, aber nicht deterministisch; der
// Test belegt die Wirkung deshalb ueber viele Wiederholungen.
func TestLos56i_ConcurrentSetValueKeepsBothKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("proxy:\n  enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 25; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := SetValue(path, "proxy.token_threshold", 111); err != nil {
				t.Errorf("writer A: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := SetValue(path, "proxy.cache_keepalive_min_messages", 222); err != nil {
				t.Errorf("writer B: %v", err)
			}
		}()
		wg.Wait()

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if !strings.Contains(content, "token_threshold: 111") || !strings.Contains(content, "cache_keepalive_min_messages: 222") {
			t.Fatalf("round %d: one writer's key was lost:\n%s", round, content)
		}
	}
}

// Cold-Review-Befund (Los 56i): NextBackupPath prueft nur os.IsNotExist. Jeder
// andere Stat-Fehler (ENOTDIR, EACCES, ELOOP) gilt damit als "Name frei", und
// die Schleife dreht weiter, ohne je einen freien Namen zu finden.
func TestLos56i_NextBackupPathReportsAnUnusablePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := NextBackupPath(filepath.Join(file, "config.yaml"))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("NextBackupPath reported a usable name below a regular file")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("NextBackupPath spun on a path it cannot stat")
	}
}
