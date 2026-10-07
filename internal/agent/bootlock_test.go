package agent

import (
	"os"
	"path/filepath"
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
