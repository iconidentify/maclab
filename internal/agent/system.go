package agent

import (
	"context"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

// Fetcher downloads an artifact by sha256 into dst and verifies it.
type Fetcher func(ctx context.Context, sha, dst string) error

// System is everything the agent does to the machine. Linux is the real
// implementation; Sim stands in for a Mac in tests.
type System interface {
	Identity() (bootID, kernel, cmdline string)
	Facts() api.Facts
	Health() api.Health
	// WatchKernel feeds every kernel log line of this boot to fn until ctx ends.
	WatchKernel(ctx context.Context, fn func(line string))
	// WatchSleep calls fn(true) just before the machine suspends and
	// fn(false) once it has resumed.
	WatchSleep(ctx context.Context, fn func(sleeping bool))

	Stage(ctx context.Context, job string, a api.StageArgs, fetch Fetcher) (api.StageResult, error)
	BootOnce(entry string) error
	RunTest(ctx context.Context, job string, t api.TestSpec, fetch Fetcher, outDir string) api.TestResult
	Collect(ctx context.Context, boot, outDir string) error
	Cleanup(job string, all bool) error
	// Screenshot captures the GUI session as JPEG to path.
	Screenshot(ctx context.Context, path string, wait time.Duration) (w, h int, err error)
	Logs(ctx context.Context, a api.LogsArgs) (string, error)
	// KernelConfig is the running kernel's .config (/proc/config.gz).
	KernelConfig() (string, error)
	Exec(ctx context.Context, a api.ExecArgs) api.ExecResult
	// RecordKnownGood records the running kernel as known-good, as setup does,
	// if it is kernel and not a lab boot.
	RecordKnownGood(kernel string) error

	// Reboot and Crash run after the command's result has been reported.
	Reboot() error
	Crash(mode string) error

	// Alive is false while a simulated machine is hung. Always true on Linux.
	Alive() bool
}
