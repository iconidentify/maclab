package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Omarchy on Apple Silicon boots U-Boot -> Limine (ESP:/EFI/BOOT/BOOTAA64.EFI)
// -> a unified kernel image on the ESP. Limine reads only FAT, so the lab
// stages each test kernel as a UKI under <esp>/maclab/<job>/ and lists it in
// a marked block at the end of limine.conf. The one-shot is the Boot Loader
// Interface's LoaderEntryOneShot variable: Limine deletes it as it reads it,
// before booting anything, so a test kernel that hangs or panics comes back
// on the default entry. Apple Silicon's U-Boot keeps variables in RAM at
// runtime and names the ESP file it loads them from (RTStorageVolatile); the
// lab writes U-Boot's serialized store (VarToFile) there, as U-Boot documents.

const (
	limineBegin    = "# maclab: begin. Managed by lab-agent: staged test kernels, booted once via LoaderEntryOneShot."
	limineEnd      = "# maclab: end"
	limineEntry    = "entry.limine"
	limineStageDir = "maclab"
	limineMinFree  = 120 << 20 // one test UKI is about 50 MB
)

var errNoLimine = errors.New("the ESP's EFI loader is not Limine")

type limineLayout struct {
	ESP     mount
	Loader  string // <esp>/EFI/BOOT/BOOTAA64.EFI
	Conf    string // the limine.conf Limine reads
	Stage   string // <esp>/maclab
	VarFile string // U-Boot's variable file on the ESP; "" where the firmware stores variables itself
}

// limineConfPaths is where Limine looks for its config on the volume it was
// loaded from, in its own order (CONFIG.md, "Location of the config file").
var limineConfPaths = []string{"EFI/BOOT/limine.conf", "boot/limine/limine.conf", "boot/limine.conf", "limine/limine.conf", "limine.conf"}

func findESP() (mount, error) {
	for _, d := range []string{"/boot/efi", "/efi", "/boot"} {
		m, err := findMount(d)
		if err == nil && m.Target == d && m.FSType == "vfat" {
			return m, nil
		}
	}
	return mount{}, fmt.Errorf("no FAT ESP mounted at /boot/efi, /efi or /boot")
}

func detectLimine() (*limineLayout, error) {
	esp, err := findESP()
	if err != nil {
		return nil, errNoLimine
	}
	l := &limineLayout{ESP: esp, Loader: filepath.Join(esp.Target, "EFI/BOOT/BOOTAA64.EFI"), Stage: filepath.Join(esp.Target, limineStageDir)}
	bin, err := os.ReadFile(l.Loader)
	if err != nil || !bytes.Contains(bin, []byte("limine.conf")) {
		return nil, errNoLimine
	}
	for _, p := range limineConfPaths {
		if _, err := os.Stat(filepath.Join(esp.Target, p)); err == nil {
			l.Conf = filepath.Join(esp.Target, p)
			break
		}
	}
	if l.Conf == "" {
		return nil, fmt.Errorf("Limine is the EFI loader but there is no limine.conf on the ESP")
	}
	if _, data, err := readEfivar("RTStorageVolatile", ubootVarGUID); err == nil {
		name := strings.TrimRight(string(data), "\x00")
		if name == "" || strings.Contains(name, "..") {
			return nil, fmt.Errorf("U-Boot names an unusable variable file %q", name)
		}
		l.VarFile = filepath.Join(esp.Target, name)
	}
	return l, nil
}

func (l *limineLayout) name() string { return "limine" }

func (l *limineLayout) problems() []string {
	var p []string
	if _, err := os.ReadFile(l.Conf); err != nil {
		p = append(p, "cannot read "+l.Conf+": "+err.Error())
	}
	if st, err := os.Stat(efivarsDir); err != nil || !st.IsDir() {
		p = append(p, "no efivarfs at "+efivarsDir+": the one-shot needs EFI variables")
	}
	if l.VarFile != "" {
		if _, _, err := readEfivar("VarToFile", ubootVarGUID); err != nil {
			p = append(p, "U-Boot keeps variables in RAM but VarToFile is unreadable: "+err.Error())
		}
	}
	if _, err := os.Stat(l.Stage); err != nil {
		p = append(p, "missing stage directory "+l.Stage+" (run lab-agent setup)")
	}
	return p
}

// install creates the stage directory and the (empty) entry block, and keeps a
// copy of U-Boot's variable file. It is idempotent.
func (l *limineLayout) install() error {
	if err := os.MkdirAll(l.Stage, 0o755); err != nil {
		return err
	}
	if l.VarFile != "" {
		bak := l.VarFile + ".pre-maclab"
		if _, err := os.Stat(bak); os.IsNotExist(err) {
			if b, err := os.ReadFile(l.VarFile); err == nil {
				if err := writeSynced(bak, b); err != nil {
					return err
				}
			}
		}
	}
	return l.writeEntries()
}

func (l *limineLayout) entryText(job, title string) string {
	return fmt.Sprintf("/%s%s\n    comment: %s\n    protocol: efi\n    path: boot():/%s/%s/uki.efi\n",
		entryPrefix, job, strings.NewReplacer("\n", " ", "#", "").Replace(title), limineStageDir, job)
}

var reMaclabBlock = regexp.MustCompile(`(?s)\n?` + regexp.QuoteMeta(limineBegin) + `.*?` + regexp.QuoteMeta(limineEnd) + `\n?`)

// writeEntries rewrites the marked block at the end of limine.conf from every
// staged job, leaving everything else in the file as it was.
// bootPartitionLock is the lock limine-snapper-sync, limine-entry-tool and the
// rest of Omarchy's Limine tooling take (/usr/lib/limine/limine-mutex) before
// they touch limine.conf or other files on the ESP. Its README asks every other
// tool to take it too. Without it, a snapshot sync rewriting limine.conf could
// interleave with ours and drop the lab's block, staged entry included.
var bootPartitionLock = "/run/lock/boot-partition.lock"

func lockBootPartition(timeout time.Duration) (func(), error) {
	os.MkdirAll(filepath.Dir(bootPartitionLock), 0o755)
	f, err := os.OpenFile(bootPartitionLock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%s has been held by another tool (limine-snapper-sync?) for over %s", bootPartitionLock, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}

func (l *limineLayout) writeEntries() error {
	unlock, err := lockBootPartition(time.Minute)
	if err != nil {
		return err
	}
	defer unlock()
	return l.writeEntriesLocked()
}

func (l *limineLayout) writeEntriesLocked() error {
	conf, err := os.ReadFile(l.Conf)
	if err != nil {
		return err
	}
	files, _ := filepath.Glob(filepath.Join(l.Stage, "*", limineEntry))
	sort.Strings(files)
	var b strings.Builder
	b.WriteString(limineBegin + "\n")
	for _, f := range files {
		if t, err := os.ReadFile(f); err == nil {
			b.Write(t)
		}
	}
	b.WriteString(limineEnd + "\n")
	rest := strings.TrimRight(reMaclabBlock.ReplaceAllString(string(conf), "\n"), "\n")
	out := rest + "\n\n" + b.String()
	if out == string(conf) {
		return nil
	}
	return writeSynced(l.Conf, []byte(out))
}

func (l *limineLayout) arm(entry string) error {
	if !strings.HasPrefix(entry, entryPrefix) {
		return fmt.Errorf("refusing to arm non-lab entry %q", entry)
	}
	job := strings.TrimPrefix(entry, entryPrefix)
	if _, err := os.Stat(filepath.Join(l.Stage, job, "uki.efi")); err != nil {
		if _, err := os.Stat(filepath.Join(l.Stage, job, installedMarker)); err != nil {
			return fmt.Errorf("entry %s is not staged", entry)
		}
	}
	unlock, err := lockBootPartition(time.Minute)
	if err != nil {
		return err
	}
	defer unlock()
	// limine-update and snapshot syncs rewrite limine.conf: put the block back.
	if err := l.writeEntriesLocked(); err != nil {
		return err
	}
	conf, _ := os.ReadFile(l.Conf)
	if !bytes.Contains(conf, []byte("\n/"+entry+"\n")) {
		return fmt.Errorf("limine.conf has no /%s entry", entry)
	}
	if err := writeEfivar("LoaderEntryOneShot", bliGUID, efiVarNV|efiVarBS|efiVarRT, utf16z(entry)); err != nil {
		return err
	}
	if err := l.persist(); err != nil {
		return err
	}
	if got := l.armed(); got != entry {
		return fmt.Errorf("one-shot readback: got %q, want %q", got, entry)
	}
	if l.VarFile != "" {
		if b, err := os.ReadFile(l.VarFile); err != nil || !bytes.Contains(b, utf16z(entry)) {
			return fmt.Errorf("one-shot is not in %s, so U-Boot would not see it", l.VarFile)
		}
	}
	return nil
}

func (l *limineLayout) armed() string {
	_, data, err := readEfivar("LoaderEntryOneShot", bliGUID)
	if err != nil {
		return ""
	}
	return fromUTF16z(data)
}

func (l *limineLayout) disarm() error {
	if l.armed() == "" {
		return nil
	}
	unlock, err := lockBootPartition(time.Minute)
	if err != nil {
		return err
	}
	defer unlock()
	if err := deleteEfivar("LoaderEntryOneShot", bliGUID); err != nil {
		return err
	}
	return l.persist()
}

// persist writes U-Boot's runtime variable store to the ESP file U-Boot loads
// at the next boot. Firmware with its own storage needs nothing.
func (l *limineLayout) persist() error {
	if l.VarFile == "" {
		return nil
	}
	_, data, err := readEfivar("VarToFile", ubootVarGUID)
	if err != nil {
		return fmt.Errorf("read U-Boot's VarToFile: %w", err)
	}
	if len(data) < 16 || !bytes.Equal(data[8:15], []byte("UbEfiVa")) {
		return fmt.Errorf("U-Boot's VarToFile does not hold a variable file")
	}
	return writeSynced(l.VarFile, data)
}

// writeUKI builds a unified kernel image for krel from image, carrying cmdline.
func writeUKI(ctx context.Context, krel, image, cmdline, out string) error {
	ictx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	tmp := out + ".tmp"
	os.Remove(tmp)
	// A fresh, exclusively created file: root must not follow a link that a
	// local user planted at a predictable /tmp path.
	f, err := os.CreateTemp("", "maclab-cmdline-*")
	if err != nil {
		return err
	}
	cmdFile := f.Name()
	defer os.Remove(cmdFile)
	_, werr := f.WriteString(cmdline + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	var cmd *exec.Cmd
	if _, err := exec.LookPath("mkinitcpio"); err == nil {
		cmd = exec.CommandContext(ictx, "mkinitcpio", "-k", krel, "-U", tmp, "--kernelimage", image, "--cmdline", cmdFile)
	} else {
		cmd = exec.CommandContext(ictx, "dracut", "--force", "--uefi", "--kver", krel, "--kernel-image", image, "--kernel-cmdline", cmdline, tmp)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("UKI: %v: %s", err, lastLines(string(out), 15))
	}
	if err := syncFile(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}

// writeSynced replaces path atomically and flushes it, for files on the FAT
// ESP that the firmware or bootloader reads at the next boot.
func writeSynced(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncFile(filepath.Dir(path))
}

func syncFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
