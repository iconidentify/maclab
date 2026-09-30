package detect

import "testing"

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"Kernel panic - not syncing: Attempted to kill init! exitcode=0x0000000b":              Panic,
		"Unable to handle kernel NULL pointer dereference at virtual address 0000000000000008": Oops,
		"Internal error: Oops: 0000000096000004 [#1] PREEMPT SMP":                              Oops,
		"watchdog: BUG: soft lockup - CPU#3 stuck for 26s! [kworker/3:1:123]":                  SoftLockup,
		"rcu: INFO: rcu_preempt self-detected stall on CPU":                                    RCUStall,
		"INFO: task kworker/u16:2:77 blocked for more than 122 seconds.":                       HungTask,
		"BUG: sleeping function called from invalid context at kernel/locking/mutex.c:580":     Bug,
		"WARNING: CPU: 2 PID: 1 at drivers/gpu/drm/asahi/foo.c:10 bar+0x10/0x20":               Warning,
		"apple-dart 382f00000.dart: DART fault":                                                "",
		"usb 1-1: new high-speed USB device number 2 using xhci_hcd":                           "",
	}
	for line, want := range cases {
		if got := Classify(line); got != want {
			t.Errorf("%q: got %q want %q", line, got, want)
		}
	}
	if Marker("[    0.000000] Booting Linux on physical CPU 0x0000000000 [0x611f0221]") != MarkKernel {
		t.Error("kernel marker")
	}
	if Marker("m1air login: ") != MarkUserspace {
		t.Error("login marker")
	}
}

func TestNewLines(t *testing.T) {
	base := "[    1.234] apple-dart 382f00000.dart: DART fault at 0x1000\nfoo: probe of 1-1 failed with error -22\n"
	got := "[    2.5] apple-dart 382f00000.dart: DART fault at 0x2000\nfoo: probe of 1-2 failed with error -22\nnew: driver regressed\n"
	n := NewLines(base, got)
	if len(n) != 1 || n[0] != "new: driver regressed" {
		t.Fatalf("%q", n)
	}
}

func TestNewLinesIgnoresFirewallNoise(t *testing.T) {
	got := "[UFW BLOCK] IN=wlan0 OUT= SRC=192.0.2.28 DST=192.0.2.33 PROTO=UDP\naudit: type=1131 audit(1.2:3): pid=1\nreal: regression\n"
	if n := NewLines("", got); len(n) != 1 || n[0] != "real: regression" {
		t.Fatalf("%q", n)
	}
}
