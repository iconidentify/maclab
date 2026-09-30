package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

// Sim is a pretend Mac. A kernel artifact for it is a text file such as
// "krel=7.1-test behavior=panic"; behaviors are ok, panic (at boot),
// hang (at boot), silent (hang with no serial output) and crash-in-test.
// A test named suspend-* puts it to sleep for SuspendFor, announcing it the way
// logind does; one named quiet-suspend-* sleeps without announcing it.
type Sim struct {
	RebootDelay time.Duration
	OnBoot      func() // called when the machine goes down, like a dropped connection
	Model       string
	Serial      func(line string) // wired to a fake OOB controller
	WorkDir     string
	SuspendFor  time.Duration

	mu       sync.Mutex
	sleepFn  func(bool)
	bootID   string
	kernel   string
	cmdline  string
	alive    bool
	armed    string
	staged   map[string]simEntry
	behavior string
	gen      int
	kmsg     chan string
	Boots    int
}

type simEntry struct{ krel, behavior, cmdline string }

const SimKnownGood = "7.1.12-good"

func NewSim(workDir string) *Sim {
	s := &Sim{RebootDelay: 100 * time.Millisecond, staged: map[string]simEntry{}, WorkDir: workDir}
	s.bootID = newID()
	s.kernel = SimKnownGood
	s.cmdline = "root=UUID=sim rw"
	s.alive = true
	s.kmsg = make(chan string, 100)
	return s
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Sim) say(line string) {
	if s.Serial != nil {
		s.Serial(line)
	}
}

func (s *Sim) Identity() (string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bootID, s.kernel, s.cmdline
}

func (s *Sim) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alive
}

func (s *Sim) Facts() api.Facts {
	model := s.Model
	if model == "" {
		model = "Simulated Mac"
	}
	return api.Facts{Hostname: "sim", Machine: "aarch64", DTModel: model, Bootloader: "grub",
		Initramfs: "mkinitcpio", GUIUser: "sim", Watchdog: true, WatchdogArmed: true, PanicTimeout: 10, PreflightOK: true}
}

func (s *Sim) Health() api.Health {
	_, _, c := s.Identity()
	return api.Health{Uptime: 1, SystemState: "running", JobTag: jobTag(c)}
}

func (s *Sim) WatchKernel(ctx context.Context, fn func(string)) {
	for {
		select {
		case <-ctx.Done():
			return
		case l := <-s.kmsg:
			fn(l)
		}
	}
}

func (s *Sim) Stage(ctx context.Context, job string, a api.StageArgs, fetch Fetcher) (api.StageResult, error) {
	e := simEntry{krel: SimKnownGood, behavior: "ok"}
	if a.Artifact != "" {
		p := filepath.Join(s.WorkDir, job+".art")
		if err := fetch(ctx, a.Artifact, p); err != nil {
			return api.StageResult{}, err
		}
		b, _ := os.ReadFile(p)
		for _, f := range strings.Fields(string(b)) {
			if v, ok := strings.CutPrefix(f, "krel="); ok {
				e.krel = v
			}
			if v, ok := strings.CutPrefix(f, "behavior="); ok {
				e.behavior = v
			}
		}
		if e.krel == SimKnownGood {
			return api.StageResult{}, fmt.Errorf("artifact kernel release %s is the running kernel", e.krel)
		}
	}
	e.cmdline = testCmdline(s.cmdline, a, job)
	s.mu.Lock()
	s.staged[job] = e
	s.mu.Unlock()
	return api.StageResult{KernelRelease: e.krel, Entry: entryPrefix + job, Cmdline: e.cmdline}, nil
}

func (s *Sim) BootOnce(entry string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.staged[strings.TrimPrefix(entry, entryPrefix)]; !ok {
		return fmt.Errorf("entry %s is not staged", entry)
	}
	s.armed = entry
	return nil
}

func (s *Sim) Reboot() error { s.boot("reboot: Restarting system"); return nil }

func (s *Sim) Crash(mode string) error {
	s.boot("Kernel panic - not syncing: sysrq triggered crash")
	return nil
}

// PowerCycle is what the OOB reset (or a human) does.
func (s *Sim) PowerCycle() { s.boot("") }

func (s *Sim) boot(last string) {
	s.mu.Lock()
	s.alive = false
	s.gen++
	gen := s.gen
	down := s.OnBoot
	s.mu.Unlock()
	if down != nil {
		down()
	}
	if last != "" {
		s.say(last)
	}
	go func() {
		time.Sleep(s.RebootDelay)
		s.mu.Lock()
		if gen != s.gen {
			s.mu.Unlock()
			return
		}
		e := simEntry{krel: SimKnownGood, behavior: "ok", cmdline: "root=UUID=sim rw"}
		if s.armed != "" {
			e = s.staged[strings.TrimPrefix(s.armed, entryPrefix)]
			s.armed = "" // GRUB clears the one-shot before booting
		}
		s.bootID = newID()
		s.Boots++
		s.mu.Unlock()
		s.say("m1n1: booting")
		if e.behavior != "silent" {
			s.say("Booting Linux on physical CPU 0x0000000000 [0x611f0221]")
		}
		switch e.behavior {
		case "panic":
			s.say("Kernel panic - not syncing: VFS: Unable to mount root fs")
			s.say("Rebooting in 10 seconds..")
			time.Sleep(s.RebootDelay)
			s.boot("")
			return
		case "hang":
			s.say("rcu: INFO: rcu_sched self-detected stall on CPU")
			return
		case "silent":
			return
		}
		s.mu.Lock()
		if gen != s.gen {
			s.mu.Unlock()
			return
		}
		s.kernel, s.cmdline, s.behavior = e.krel, e.cmdline, e.behavior
		s.alive = true
		s.mu.Unlock()
		s.say("Run /init as init process")
		s.say("sim login: ")
	}()
}

func (s *Sim) WatchSleep(ctx context.Context, fn func(bool)) {
	s.mu.Lock()
	s.sleepFn = fn
	s.mu.Unlock()
}

// suspend sleeps the machine for d: no heartbeats, no reports, nothing lost.
func (s *Sim) suspend(d time.Duration, announce bool) {
	s.mu.Lock()
	fn := s.sleepFn
	s.mu.Unlock()
	if announce && fn != nil {
		fn(true)
	}
	s.mu.Lock()
	s.alive = false
	s.mu.Unlock()
	time.Sleep(d)
	s.mu.Lock()
	s.alive = true
	s.mu.Unlock()
	if announce && fn != nil {
		fn(false)
	}
}

func (s *Sim) RunTest(ctx context.Context, job string, t api.TestSpec, fetch Fetcher, outDir string) api.TestResult {
	s.mu.Lock()
	b := s.behavior
	s.mu.Unlock()
	if strings.HasPrefix(t.Name, "suspend-") || strings.HasPrefix(t.Name, "quiet-suspend-") {
		s.suspend(s.SuspendFor, strings.HasPrefix(t.Name, "suspend-"))
	}
	if b == "crash-in-test" {
		s.kmsg <- "Unable to handle kernel NULL pointer dereference at virtual address 0000000000000008"
		time.Sleep(50 * time.Millisecond)
		s.boot("Kernel panic - not syncing: Oops: Fatal exception")
		<-ctx.Done()
		return api.TestResult{Name: t.Name, Error: "machine went away"}
	}
	os.WriteFile(filepath.Join(outDir, "log.txt"), []byte("sim test "+t.Name+" ok\n"), 0o644)
	passed := !strings.Contains(t.Name, "fail")
	code := 0
	if !passed {
		code = 1
	}
	return api.TestResult{Name: t.Name, Passed: passed, ExitCode: code, Seconds: 0.01, Tail: "sim test " + t.Name}
}

func (s *Sim) Collect(ctx context.Context, boot, outDir string) error {
	s.mu.Lock()
	k := s.kernel
	s.mu.Unlock()
	errs := "apple-dart 382f00000.dart: DART fault\n"
	if k != SimKnownGood {
		errs += "sim: new regression warning in driver foo\n"
	}
	os.WriteFile(filepath.Join(outDir, "dmesg-errors.txt"), []byte(errs), 0o644)
	os.WriteFile(filepath.Join(outDir, "journal.txt"), []byte("sim journal for boot "+boot+"\n"), 0o644)
	return nil
}

func (s *Sim) Screenshot(ctx context.Context, path string, wait time.Duration) (int, int, error) {
	s.mu.Lock()
	k := s.kernel
	s.mu.Unlock()
	const w, h = 640, 400
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// A desktop-ish frame: dark background, a bar, and a window whose tint encodes the kernel.
	tint := uint8(len(k) * 37)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{28, 34, 53, 255}
			switch {
			case y < 16:
				c = color.RGBA{20, 24, 38, 255}
			case x > 60 && x < 420 && y > 60 && y < 320:
				c = color.RGBA{40, 48 + tint/8, 72, 255}
			case x > 440 && x < 600 && y > 60 && y < 200:
				c = color.RGBA{154, 179, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	return w, h, jpeg.Encode(f, img, &jpeg.Options{Quality: 80})
}

func (s *Sim) Logs(ctx context.Context, a api.LogsArgs) (string, error) {
	_, k, _ := s.Identity()
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "Sep 30 00:%02d:%02d.000000 sim kernel: sim %s line %d from %s\n", i/60, i%60, a.Source, i, k)
	}
	return b.String(), nil
}

func (s *Sim) Cleanup(job string, all bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if all {
		s.staged = map[string]simEntry{}
		s.armed = ""
	} else {
		delete(s.staged, job)
	}
	return nil
}

func (s *Sim) KernelConfig() (string, error) {
	_, k, _ := s.Identity()
	return "# sim config for " + k + "\nCONFIG_ARM64=y\nCONFIG_LOCALVERSION=\"\"\n", nil
}

func (s *Sim) Exec(ctx context.Context, a api.ExecArgs) api.ExecResult {
	return api.ExecResult{Stdout: "sim ran: " + a.Command + "\n", Via: "agent"}
}
