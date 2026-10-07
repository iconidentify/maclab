package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The lab's lock must exclude limine-mutex holders (flock on the same file),
// whichever of the two lock files their version uses, and wait for them rather
// than fail at once.
func TestBootPartitionLockWaitsForOtherHolders(t *testing.T) {
	for i := range []string{"boot-partition.lock", "limine-global.lock"} {
		t.Run(filepath.Base([]string{"/run/lock/boot-partition.lock", "/tmp/limine-global.lock"}[i]), func(t *testing.T) {
			dir := t.TempDir()
			bootPartitionLocks = []string{filepath.Join(dir, "boot-partition.lock"), filepath.Join(dir, "limine-global.lock")}
			waitsFor(t, bootPartitionLocks[i])
		})
	}
}

func waitsFor(t *testing.T, held string) {
	other, err := os.OpenFile(held, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if _, err := lockBootPartition(300 * time.Millisecond); err == nil {
		t.Fatal("got the lock while another tool held it")
	}
	fd := int(other.Fd())
	released := make(chan struct{})
	go func() { time.Sleep(300 * time.Millisecond); unix.Flock(fd, unix.LOCK_UN); close(released) }()
	defer func() { <-released }()
	start := time.Now()
	unlock, err := lockBootPartition(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("did not wait for the other holder")
	}
	unlock()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal("lock not released:", err)
	}
	for _, p := range bootPartitionLocks { // both are free again
		f, _ := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
		if p != held && unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
			t.Fatalf("%s not released", p)
		}
		f.Close()
	}
}

// A lock path that isn't a plain file is refused at once: a symlink planted in
// /tmp is not followed (its target is not created), and a FIFO or a directory is
// refused as not regular. The one-second guard only keeps a regression from
// hanging the test.
func TestBootPartitionLockRefusesNonRegularFiles(t *testing.T) {
	for name, plant := range map[string]func(p, dir string) error{
		"symlink":   func(p, dir string) error { return os.Symlink(filepath.Join(dir, "victim"), p) },
		"fifo":      func(p, _ string) error { return unix.Mkfifo(p, 0o644) },
		"directory": func(p, _ string) error { return os.Mkdir(p, 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bootPartitionLocks = []string{filepath.Join(dir, "boot-partition.lock"), filepath.Join(dir, "limine-global.lock")}
			if err := plant(bootPartitionLocks[1], dir); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				unlock, err := lockBootPartition(2 * time.Second)
				if err == nil {
					unlock()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("locked through a non-regular file")
				}
			case <-time.After(time.Second):
				t.Fatal("lockBootPartition did not return within the guard")
			}
			if _, err := os.Lstat(filepath.Join(dir, "victim")); err == nil {
				t.Fatal("followed the symlink and created its target")
			}
			// the first lock was released
			f, _ := os.OpenFile(bootPartitionLocks[0], os.O_RDWR, 0)
			defer f.Close()
			if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal("first lock not released after the refusal")
			}
		})
	}
}

// A lock directory that can't be created is an error, and the lock already
// taken is released.
func TestBootPartitionLockReportsUncreatableDirectory(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	os.WriteFile(blocker, nil, 0o644)
	bootPartitionLocks = []string{filepath.Join(dir, "boot-partition.lock"), filepath.Join(blocker, "limine-global.lock")}
	if _, err := lockBootPartition(time.Second); err == nil || !strings.Contains(err.Error(), "not-a-dir") {
		t.Fatalf("uncreatable lock directory: %v", err)
	}
	f, _ := os.OpenFile(bootPartitionLocks[0], os.O_RDWR, 0)
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal("first lock not released after the error")
	}
}
