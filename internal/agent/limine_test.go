package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The head of Omarchy's Apple Silicon limine.conf, as limine-update writes it.
const omarchyLimineConf = `default_entry: 2
interface_branding: Omarchy Bootloader
timeout: 3

/+Omarchy
comment: Omarchy
comment: machine-id=0123456789abcdef0123456789abcdef order-priority=50
  //linux-aurora
  comment: 7.1.12-2-11.6-sep-ARCH
  protocol: efi
  path: boot():/EFI/Linux/omarchy_linux-aurora.efi
`

func testLimine(t *testing.T) *limineLayout {
	t.Helper()
	dir := t.TempDir()
	l := &limineLayout{Conf: filepath.Join(dir, "limine.conf"), Stage: filepath.Join(dir, "maclab")}
	if err := os.WriteFile(l.Conf, []byte(omarchyLimineConf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.Stage, 0o755); err != nil {
		t.Fatal(err)
	}
	return l
}

func stageLimineEntry(t *testing.T, l *limineLayout, job string) {
	t.Helper()
	d := filepath.Join(l.Stage, job)
	os.MkdirAll(d, 0o755)
	if err := os.WriteFile(filepath.Join(d, limineEntry), []byte(l.entryText(job, "maclab "+job+": 7.1.12-lab")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLimineEntries(t *testing.T) {
	l := testLimine(t)
	stageLimineEntry(t, l, "j1001-120000-abcd")
	if err := l.writeEntries(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(l.Conf)
	want := omarchyLimineConf + "\n" + limineBegin + "\n" +
		"/maclab-j1001-120000-abcd\n    comment: maclab j1001-120000-abcd: 7.1.12-lab\n    protocol: efi\n    path: boot():/maclab/j1001-120000-abcd/uki.efi\n" +
		limineEnd + "\n"
	if string(got) != want {
		t.Fatalf("limine.conf:\n%s\nwant:\n%s", got, want)
	}

	// Idempotent, and a second job lands in the same block.
	stageLimineEntry(t, l, "j1001-130000-ef01")
	l.writeEntries()
	l.writeEntries()
	got, _ = os.ReadFile(l.Conf)
	if n := strings.Count(string(got), limineBegin); n != 1 {
		t.Fatalf("%d maclab blocks", n)
	}
	if !strings.Contains(string(got), "\n/maclab-j1001-130000-ef01\n") || !strings.HasPrefix(string(got), omarchyLimineConf) {
		t.Fatalf("limine.conf:\n%s", got)
	}

	// Cleaning up every job leaves an empty block and Omarchy's menu as it was.
	os.RemoveAll(filepath.Join(l.Stage, "j1001-120000-abcd"))
	os.RemoveAll(filepath.Join(l.Stage, "j1001-130000-ef01"))
	l.writeEntries()
	got, _ = os.ReadFile(l.Conf)
	if want := omarchyLimineConf + "\n" + limineBegin + "\n" + limineEnd + "\n"; string(got) != want {
		t.Fatalf("limine.conf:\n%s", got)
	}
}

// limine-update rewrites the menu on a kernel update and may move or drop the
// block; the next writeEntries puts exactly one back at the end.
func TestLimineEntriesAfterRewrite(t *testing.T) {
	l := testLimine(t)
	stageLimineEntry(t, l, "j1")
	l.writeEntries()
	conf, _ := os.ReadFile(l.Conf)
	moved := strings.Replace(string(conf), "/+Omarchy", "/+Omarchy\n  //linux-aurora-fallback\n  protocol: efi\n  path: boot():/EFI/Linux/omarchy_linux-aurora-fallback.efi", 1)
	os.WriteFile(l.Conf, []byte(moved), 0o644)
	l.writeEntries()
	got, _ := os.ReadFile(l.Conf)
	if n := strings.Count(string(got), limineBegin); n != 1 || !strings.HasSuffix(string(got), limineEnd+"\n") {
		t.Fatalf("limine.conf:\n%s", got)
	}
	if !strings.Contains(string(got), "linux-aurora-fallback") {
		t.Fatal("lost limine-update's own entries")
	}
}

func TestUTF16z(t *testing.T) {
	b := utf16z("maclab-j1")
	if len(b) != 2*len("maclab-j1")+2 || b[0] != 'm' || b[1] != 0 || b[len(b)-1] != 0 || b[len(b)-2] != 0 {
		t.Fatalf("% x", b)
	}
	if got := fromUTF16z(b); got != "maclab-j1" {
		t.Fatal(got)
	}
}
