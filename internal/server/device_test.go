package server

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

// A soft reboot queued for a stuck boot must not reach the fresh boot that
// replaced it: that would reboot a healthy test kernel mid-test.
func TestCommandsStayWithTheirBoot(t *testing.T) {
	s := &Server{cfg: Config{Interval: time.Second}, log: log.New(io.Discard, "", 0), ctx: context.Background()}
	st, err := openStore(t.TempDir() + "/db")
	if err != nil {
		t.Fatal(err)
	}
	s.store = st
	v := newDev(s, api.Device{Name: "m", ID: "d"}, "h")

	v.heartbeat(api.Checkin{BootID: "boot-A"})
	id := v.send(api.CmdReboot, "", nil)

	v.heartbeat(api.Checkin{BootID: "boot-B"}) // the Mac came back on its own
	if resp := v.deliver("boot-B"); len(resp.Commands) != 0 {
		t.Fatalf("boot-B received %v, a command meant for boot-A", resp.Commands)
	}
	v.mu.Lock()
	r, ok := v.results[id]
	v.mu.Unlock()
	if !ok || r.Error != errDeviceLost.Error() {
		t.Fatalf("the stale command should fail as lost, got %+v (present=%v)", r, ok)
	}

	id2 := v.send(api.CmdReboot, "", nil)
	if resp := v.deliver("boot-B"); len(resp.Commands) != 1 || resp.Commands[0].ID != id2 {
		t.Fatalf("a command for boot-B must still be delivered, got %v", resp.Commands)
	}
}
