package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

// Kernel artifacts use the Arch package layout: exactly one
// usr/lib/modules/<release>/ holding the modules and vmlinuz (or Image).
// A linux-*.pkg.tar.zst works as-is; `lab pack` builds one from an O= tree.

func (l *Linux) jobDir(job string) string { return filepath.Join(l.WorkDir, "jobs", job) }

func (l *Linux) Stage(ctx context.Context, job string, a api.StageArgs, fetch Fetcher) (api.StageResult, error) {
	defer syncDisks()
	var res api.StageResult
	if strings.ContainsAny(job, "/. ") || job == "" {
		return res, fmt.Errorf("bad job id %q", job)
	}
	b, err := detectBootloader()
	if err != nil {
		return res, err
	}
	if p := b.problems(); len(p) > 0 {
		return res, fmt.Errorf("one-shot hook not ready: %s", strings.Join(p, "; "))
	}
	kg, err := l.knownGood()
	if err != nil {
		return res, err
	}
	bootDir := filepath.Join(stageDir(), job)
	if err := os.MkdirAll(bootDir, 0o755); err != nil {
		return res, err
	}
	if lim, ok := b.(*limineLayout); ok {
		return l.stageLimine(ctx, job, a, kg, lim, bootDir, fetch)
	}
	g := b.(*grubLayout)

	linux, initrd := kg.GrubLinux, kg.GrubInitrd
	uuid, fstype := g.Boot.UUID, g.Boot.FSType
	if kg.RootUUID != "" {
		uuid, fstype = kg.RootUUID, kg.RootFS
	}
	krel := kg.Kernel
	if a.Artifact != "" {
		krel, err = l.install(ctx, job, a.Artifact, fetch, func(krel, image string) error {
			return grubBootFiles(ctx, krel, image, bootDir)
		})
		if err != nil {
			l.removeJob(job)
			return res, err
		}
		var lerr error
		if linux, uuid, fstype, lerr = grubLocate(filepath.Join(bootDir, "vmlinuz")); lerr != nil {
			l.removeJob(job)
			return res, lerr
		}
		initrd = filepath.Join(filepath.Dir(linux), "initramfs.img")
	}

	cmdline, err := stageCmdline(kg.Cmdline, stockCmdline, a, job)
	if err != nil {
		l.removeJob(job)
		return res, err
	}
	title := fmt.Sprintf("maclab %s: %s", job, krel)
	os.WriteFile(filepath.Join(bootDir, "krel"), []byte(krel), 0o644)
	if err := os.WriteFile(filepath.Join(bootDir, "entry.cfg"), []byte(g.entryText(job, title, linux, initrd, cmdline, uuid, fstype)), 0o644); err != nil {
		return res, err
	}
	if err := g.writeEntries(); err != nil {
		return res, err
	}
	return api.StageResult{KernelRelease: krel, Entry: entryPrefix + job, Cmdline: cmdline}, nil
}

// stageLimine stages a job as a UKI on the ESP and lists it in limine.conf.
// Without an artifact it boots the known-good kernel through the one-shot.
func (l *Linux) stageLimine(ctx context.Context, job string, a api.StageArgs, kg *KnownGood, lim *limineLayout, bootDir string, fetch Fetcher) (api.StageResult, error) {
	var res api.StageResult
	cmdline, err := stageCmdline(kg.Cmdline, stockCmdline, a, job)
	if err != nil {
		return res, err
	}
	uki := filepath.Join(bootDir, "uki.efi")
	krel := kg.Kernel
	if a.Artifact != "" {
		krel, err = l.install(ctx, job, a.Artifact, fetch, func(krel, image string) error {
			return writeUKI(ctx, krel, image, cmdline, uki)
		})
	} else {
		err = writeUKI(ctx, krel, filepath.Join("/usr/lib/modules", krel, "vmlinuz"), cmdline, uki)
	}
	if err != nil {
		l.removeJob(job)
		lim.writeEntries()
		return res, err
	}
	title := fmt.Sprintf("maclab %s: %s", job, krel)
	os.WriteFile(filepath.Join(bootDir, "krel"), []byte(krel), 0o644)
	if err := os.WriteFile(filepath.Join(bootDir, limineEntry), []byte(lim.entryText(job, title)), 0o644); err != nil {
		return res, err
	}
	if err := lim.writeEntries(); err != nil {
		return res, err
	}
	return api.StageResult{KernelRelease: krel, Entry: entryPrefix + job, Cmdline: cmdline}, nil
}

// stageCmdline is the cmdline a test boot gets. It starts from a base: the
// known-good kernel's cmdline, the distro's stock one ("default"), or a literal.
// It removes the base parameters a.CmdlineStrip names, then adds the lab's own
// (testCmdline). Proving a kernel needs none of a Mac's bring-up parameters is
// "default", or the known-good base with those parameters stripped.
func stageCmdline(knownGood string, stock func() (string, error), a api.StageArgs, job string) (string, error) {
	base := knownGood
	switch b := strings.TrimSpace(a.CmdlineBase); b {
	case "", "known-good":
	case "default":
		s, err := stock()
		if err != nil {
			return "", err
		}
		base = s
	default:
		base = b
	}
	base = stripParams(base, a.CmdlineStrip)
	if !hasParam(base, "root") {
		return "", fmt.Errorf("the test cmdline would have no root= (base %q, strip %q)", a.CmdlineBase, a.CmdlineStrip)
	}
	return testCmdline(base, a, job), nil
}

// stripParams drops the parameters whose name (before "=") or whole text
// matches one of the globs.
func stripParams(cmdline string, globs []string) string {
	var out []string
	for _, f := range strings.Fields(cmdline) {
		name, _, _ := strings.Cut(f, "=")
		drop := false
		for _, g := range globs {
			if m, _ := path.Match(g, name); m {
				drop = true
			} else if m, _ := path.Match(g, f); m {
				drop = true
			}
		}
		if !drop {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

func hasParam(cmdline, name string) bool {
	for _, f := range strings.Fields(cmdline) {
		if k, _, _ := strings.Cut(f, "="); k == name {
			return true
		}
	}
	return false
}

var reStockCmdline = regexp.MustCompile(`(?m)^\s*KERNEL_CMDLINE\[default\]=(["'])(.*)["']\s*$`)

// stockCmdline is the distro's default cmdline for a kernel with no entry of
// its own: KERNEL_CMDLINE[default] in /etc/default/limine on Omarchy.
func stockCmdline() (string, error) {
	b, err := os.ReadFile("/etc/default/limine")
	if err != nil {
		return "", fmt.Errorf("cmdline base \"default\" reads KERNEL_CMDLINE[default] from /etc/default/limine: %w", err)
	}
	m := reStockCmdline.FindSubmatch(b)
	if m == nil {
		return "", errors.New("no KERNEL_CMDLINE[default] in /etc/default/limine")
	}
	return string(m[2]), nil
}

// testCmdline starts from the base cmdline, makes boot verbose, makes
// panics reboot, and tags the boot so labd can tell which entry came up.
func testCmdline(base string, a api.StageArgs, job string) string {
	var out []string
	for _, f := range strings.Fields(base) {
		switch {
		case strings.HasPrefix(f, "BOOT_IMAGE="), strings.HasPrefix(f, "initrd="),
			strings.HasPrefix(f, "maclab."), strings.HasPrefix(f, "panic="),
			strings.HasPrefix(f, "loglevel="), f == "quiet", f == "splash":
			continue
		}
		out = append(out, f)
	}
	out = append(out, "loglevel=7", "panic=10")
	if a.Serial {
		out = append(out, "console=ttySAC0,115200", "console=tty0")
	}
	if s := strings.TrimSpace(a.Cmdline); s != "" {
		out = append(out, s)
	}
	return strings.Join(append(out, "maclab.job="+job), " ")
}

// install puts an artifact's modules in place and hands its kernel image to
// boot, which writes the loader's boot files while the image still exists.
func (l *Linux) install(ctx context.Context, job, sha string, fetch Fetcher, boot func(krel, image string) error) (string, error) {
	work := l.jobDir(job)
	os.RemoveAll(work)
	x := filepath.Join(work, "x")
	if err := os.MkdirAll(x, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(x)
	art := filepath.Join(work, "artifact")
	if err := fetch(ctx, sha, art); err != nil {
		return "", err
	}
	defer os.Remove(art)
	if out, err := exec.CommandContext(ctx, "tar", "-xf", art, "-C", x).CombinedOutput(); err != nil {
		return "", fmt.Errorf("extract: %v: %s", err, out)
	}

	var dirs []string
	for _, pat := range []string{"usr/lib/modules/*", "lib/modules/*"} {
		m, _ := filepath.Glob(filepath.Join(x, pat))
		for _, d := range m {
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				dirs = append(dirs, d)
			}
		}
	}
	if len(dirs) != 1 {
		return "", fmt.Errorf("artifact must contain exactly one usr/lib/modules/<release>/, found %d", len(dirs))
	}
	src := dirs[0]
	krel := filepath.Base(src)

	var image string
	for _, c := range []string{filepath.Join(src, "vmlinuz"), filepath.Join(src, "Image"), filepath.Join(x, "boot", "Image"), filepath.Join(x, "Image")} {
		if _, err := os.Stat(c); err == nil {
			image = c
			break
		}
	}
	if image == "" {
		return "", fmt.Errorf("no vmlinuz or Image in usr/lib/modules/%s/", krel)
	}

	// Never touch a kernel the lab did not install: installing over the
	// running or known-good kernel's modules is how a Mac stops booting.
	_, running, _ := l.Identity()
	if krel == running {
		return "", fmt.Errorf("artifact kernel release %s is the running kernel; build with a unique LOCALVERSION", krel)
	}
	dst := "/usr/lib/modules/" + krel
	if _, err := os.Stat(dst); err == nil {
		owner := readTrim(filepath.Join(dst, markerFile))
		if owner == "" {
			return "", fmt.Errorf("%s already exists and was not installed by the lab; build with a unique LOCALVERSION", dst)
		}
		if owner != job {
			return "", fmt.Errorf("%s belongs to lab job %s; clean that job up first", dst, owner)
		}
		os.RemoveAll(dst)
	}
	if err := os.Rename(src, dst); err != nil {
		if out, err := exec.Command("cp", "-a", src, dst).CombinedOutput(); err != nil {
			return "", fmt.Errorf("install modules: %v: %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(dst, markerFile), []byte(job), 0o644)
	if image == filepath.Join(src, filepath.Base(image)) {
		image = filepath.Join(dst, filepath.Base(image))
	}
	if out, err := exec.CommandContext(ctx, "depmod", "-a", krel).CombinedOutput(); err != nil {
		return "", fmt.Errorf("depmod: %v: %s", err, out)
	}
	if err := boot(krel, image); err != nil {
		return "", err
	}
	return krel, nil
}

// grubBootFiles copies the kernel next to its entry and builds its initramfs.
func grubBootFiles(ctx context.Context, krel, image, bootDir string) error {
	if out, err := exec.Command("cp", image, filepath.Join(bootDir, "vmlinuz")).CombinedOutput(); err != nil {
		return fmt.Errorf("copy image: %v: %s", err, out)
	}
	ictx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	initrd := filepath.Join(bootDir, "initramfs.img")
	var cmd *exec.Cmd
	if _, err := exec.LookPath("mkinitcpio"); err == nil {
		cmd = exec.CommandContext(ictx, "mkinitcpio", "-k", krel, "-g", initrd)
	} else {
		cmd = exec.CommandContext(ictx, "dracut", "--force", "--kver", krel, initrd)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("initramfs: %v: %s", err, lastLines(string(out), 15))
	}
	return nil
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// removeJob deletes everything a job staged, except modules of the running kernel.
func (l *Linux) removeJob(job string) error {
	bootDir := filepath.Join(stageDir(), job)
	krel := readTrim(filepath.Join(bootDir, "krel"))
	_, running, _ := l.Identity()
	if krel != "" && krel != running {
		dst := "/usr/lib/modules/" + krel
		if readTrim(filepath.Join(dst, markerFile)) == job {
			os.RemoveAll(dst)
		}
	}
	os.RemoveAll(bootDir)
	os.RemoveAll(l.jobDir(job))
	return nil
}

func (l *Linux) Cleanup(job string, all bool) error {
	defer syncDisks()
	g, err := detectBootloader()
	if err != nil {
		return err
	}
	_, _, cmdline := l.Identity()
	current := jobTag(cmdline)
	var jobs []string
	if all {
		dirs, _ := filepath.Glob(filepath.Join(stageDir(), "*"))
		for _, d := range dirs {
			jobs = append(jobs, filepath.Base(d))
		}
	} else if job != "" {
		jobs = []string{job}
	}
	for _, j := range jobs {
		if j == current {
			continue // still running on it
		}
		if g.armed() == entryPrefix+j {
			g.disarm()
		}
		l.removeJob(j)
	}
	if all {
		g.disarm()
	}
	return g.writeEntries()
}
