// Package detect classifies kernel log lines and diffs dmesg against a baseline.
// The same rules run on the agent (/dev/kmsg) and in labd (serial console).
package detect

import (
	"regexp"
	"strings"
)

// Event kinds. Order matters: the first matching rule wins.
const (
	Panic      = "panic"
	Oops       = "oops"
	SoftLockup = "soft_lockup"
	HardLockup = "hard_lockup"
	RCUStall   = "rcu_stall"
	HungTask   = "hung_task"
	Bug        = "bug"
	Warning    = "warning"

	// Sleep and Wake come from the agent (logind's PrepareForSleep), not from
	// kernel lines: a Mac that announced Sleep is asleep, not hung.
	Sleep = "sleep"
	Wake  = "wake"
)

// Boot progress markers, only meaningful on serial.
const (
	MarkFirmware  = "firmware"  // m1n1 / U-Boot / GRUB / Limine output
	MarkKernel    = "kernel"    // kernel has started printing
	MarkInit      = "init"      // handed off to userspace
	MarkUserspace = "userspace" // systemd reached a login prompt
	MarkReboot    = "rebooting" // orderly or panic-driven restart
)

type rule struct {
	kind string
	re   *regexp.Regexp
}

var rules = []rule{
	{Panic, regexp.MustCompile(`Kernel panic - not syncing`)},
	{Oops, regexp.MustCompile(`Internal error: Oops|Unable to handle kernel (NULL pointer|paging request)`)},
	{SoftLockup, regexp.MustCompile(`BUG: soft lockup`)},
	{HardLockup, regexp.MustCompile(`(?i)hard LOCKUP`)},
	{RCUStall, regexp.MustCompile(`rcu: INFO: rcu_\w+ (self-)?detected stall|rcu_\w+ kthread starved`)},
	{HungTask, regexp.MustCompile(`INFO: task .+ blocked for more than \d+ seconds`)},
	{Bug, regexp.MustCompile(`kernel BUG at|BUG: (KASAN|KFENCE|scheduling while atomic|sleeping function|unable to handle|Bad page|workqueue lockup)`)},
	{Warning, regexp.MustCompile(`WARNING: CPU: \d+ PID: \d+`)},
}

var markers = []rule{
	{MarkReboot, regexp.MustCompile(`reboot: Restarting system|Rebooting in \d+ seconds`)},
	{MarkUserspace, regexp.MustCompile(` login: |Reached target (Multi-User|Graphical Interface)`)},
	{MarkInit, regexp.MustCompile(`Run /\S*init as init process|systemd\[1\]: `)},
	{MarkKernel, regexp.MustCompile(`Booting Linux on physical CPU|Linux version \d`)},
	{MarkFirmware, regexp.MustCompile(`m1n1|U-Boot|GNU GRUB|Welcome to GRUB|Limine`)},
}

// Classify returns the event kind for a kernel log line, or "".
func Classify(line string) string {
	for _, r := range rules {
		if r.re.MatchString(line) {
			return r.kind
		}
	}
	return ""
}

// Marker returns the boot-progress marker a serial line shows, or "".
func Marker(line string) string {
	for _, r := range markers {
		if r.re.MatchString(line) {
			return r.kind
		}
	}
	return ""
}

// Severity ranks event kinds so the worst one names a failed job's outcome.
func Severity(kind string) int {
	switch kind {
	case Panic:
		return 9
	case Oops, HardLockup:
		return 8
	case SoftLockup, RCUStall:
		return 7
	case HungTask, Bug:
		return 6
	case Warning:
		return 3
	}
	return 0
}

// Fatal reports whether an event kind means the running kernel cannot be trusted.
func Fatal(kind string) bool { return Severity(kind) >= 6 }

var (
	reStamp = regexp.MustCompile(`^\s*\[\s*\d+\.\d+\]\s*`)
	reHex   = regexp.MustCompile(`0x[0-9a-fA-F]+|\b[0-9a-f]{8,}\b`)
	reNum   = regexp.MustCompile(`\b\d+\b`)
	rePID   = regexp.MustCompile(`\[\d+\]`)
)

// Normalize strips timestamps, addresses and counters so the same message
// from two boots compares equal.
func Normalize(line string) string {
	s := reStamp.ReplaceAllString(line, "")
	s = rePID.ReplaceAllString(s, "[N]")
	s = reHex.ReplaceAllString(s, "X")
	s = reNum.ReplaceAllString(s, "N")
	return strings.TrimSpace(s)
}

// noise is log traffic that says nothing about the kernel's health.
var noise = regexp.MustCompile(`\[UFW (BLOCK|ALLOW|AUDIT)\]|\baudit: type=|kauditd_printk_skb`)

// NewLines returns the lines of got whose normalized form never appears in
// baseline, ignoring firewall and audit traffic.
func NewLines(baseline, got string) []string {
	seen := map[string]bool{}
	for _, l := range strings.Split(baseline, "\n") {
		if n := Normalize(l); n != "" {
			seen[n] = true
		}
	}
	var out []string
	added := map[string]bool{}
	for _, l := range strings.Split(got, "\n") {
		n := Normalize(l)
		if n == "" || seen[n] || added[n] || noise.MatchString(l) {
			continue
		}
		added[n] = true
		out = append(out, strings.TrimSpace(l))
	}
	return out
}
