package oobd

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A shell echoing what someone types proves the link works: the arm loop must
// not re-arm (each re-arm injects a stray byte into their input).
func TestArmLoopLeavesATypingSessionAlone(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	tool := filepath.Join(dir, "tool")
	os.WriteFile(tool, []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n"), 0o755)
	s := New(Config{Tool: tool}, log.New(io.Discard, "", 0))

	s.mu.Lock()
	s.typed = time.Now().Add(-2 * time.Second)
	s.rx = time.Now().Add(-time.Second) // echo arrived after the typing
	s.armUntil = time.Now().Add(20 * time.Second)
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.armLoop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("arm loop kept running during an echoing session")
	}
	if b, _ := os.ReadFile(calls); len(b) > 0 {
		t.Fatalf("re-armed under a typing user: %q", b)
	}

	// Quiet console, nobody typing: re-arming is the point.
	s.mu.Lock()
	s.typed, s.rx = time.Time{}, time.Time{}
	s.armUntil = time.Now().Add(time.Second)
	s.mu.Unlock()
	s.armLoop()
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "serial") {
		t.Fatalf("a silent console was not re-armed: %q", b)
	}
}

// After a reboot the loop re-arms once; the Mac's output (U-Boot) proves the
// link is back. Re-arming again would press a "key" into GRUB's countdown.
func TestArmLoopStopsOnceTheMacTalks(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	tool := filepath.Join(dir, "tool")
	os.WriteFile(tool, []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n"), 0o755)
	s := New(Config{Tool: tool}, log.New(io.Discard, "", 0))
	s.mu.Lock()
	s.armUntil = time.Now().Add(30 * time.Second)
	s.mu.Unlock()
	go func() { // the Mac starts talking a second after the first re-arm
		time.Sleep(time.Second)
		s.emitRaw([]byte("U-Boot 2026.04\n"))
	}()
	done := make(chan struct{})
	go func() { s.armLoop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("arm loop kept going after the Mac talked")
	}
	b, _ := os.ReadFile(calls)
	if n := strings.Count(string(b), "serial"); n != 1 {
		t.Fatalf("want exactly 1 re-arm, got %d: %q", n, b)
	}
}
