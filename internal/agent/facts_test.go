package agent

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

type factsSystem struct {
	*Sim
	cleaned atomic.Bool
}

func (s *factsSystem) Facts() api.Facts {
	f := s.Sim.Facts()
	f.PreflightOK = s.cleaned.Load()
	if !f.PreflightOK {
		f.Problems = []string{"not enough staging space"}
	}
	return f
}

func (s *factsSystem) Cleanup(string, bool) error {
	s.cleaned.Store(true)
	return nil
}

func factsAgent(t *testing.T, handle func(http.ResponseWriter, api.Checkin)) *Agent {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ci api.Checkin
		if err := json.NewDecoder(r.Body).Decode(&ci); err != nil {
			t.Errorf("decode checkin: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		handle(w, ci)
	}))
	t.Cleanup(hs.Close)
	sys := &factsSystem{Sim: NewSim(t.TempDir())}
	return New(&Config{Server: hs.URL, Secret: "test"}, sys, log.New(io.Discard, "", 0))
}

func TestFactsRefreshAfterCleanup(t *testing.T) {
	checkins := make(chan api.Checkin, 5)
	var fail atomic.Bool
	a := factsAgent(t, func(w http.ResponseWriter, ci api.Checkin) {
		checkins <- ci
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "{}")
	})
	ctx := context.Background()
	if err := a.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ci := <-checkins; ci.Facts == nil || ci.Facts.PreflightOK {
		t.Fatalf("initial checkin must report failed preflight: %+v", ci.Facts)
	}
	if err := a.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ci := <-checkins; ci.Facts != nil {
		t.Fatal("unchanged facts resent immediately")
	}
	if _, _, err := a.exec(ctx, api.Command{Kind: api.CmdCleanup}); err != nil {
		t.Fatal(err)
	}
	// A failed checkin must not consume the refresh.
	fail.Store(true)
	if err := a.poll(ctx); err == nil {
		t.Fatal("expected failed checkin")
	}
	<-checkins
	fail.Store(false)
	if err := a.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ci := <-checkins; ci.Facts == nil || !ci.Facts.PreflightOK || len(ci.Facts.Problems) != 0 {
		t.Fatalf("cleanup must refresh preflight without restarting: %+v", ci.Facts)
	}
}

func TestFactsRefreshDuringCheckin(t *testing.T) {
	checkins := make(chan api.Checkin, 2)
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int32
	a := factsAgent(t, func(w http.ResponseWriter, ci api.Checkin) {
		checkins <- ci
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		io.WriteString(w, "{}")
	})
	done := make(chan error, 1)
	go func() { done <- a.poll(ctx) }()
	select {
	case <-checkins:
	case <-ctx.Done():
		t.Fatal("initial checkin did not arrive")
	}
	if _, _, err := a.exec(ctx, api.Command{Kind: api.CmdCleanup}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := a.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ci := <-checkins; ci.Facts == nil || !ci.Facts.PreflightOK {
		t.Fatalf("in-flight checkin lost cleanup refresh: %+v", ci.Facts)
	}
}

func TestFactsRefreshPeriodically(t *testing.T) {
	checkins := make(chan api.Checkin, 2)
	a := factsAgent(t, func(w http.ResponseWriter, ci api.Checkin) {
		checkins <- ci
		io.WriteString(w, "{}")
	})
	ctx := context.Background()
	if err := a.poll(ctx); err != nil {
		t.Fatal(err)
	}
	<-checkins
	// Space can also change outside lab commands, such as snapshot pruning.
	a.sys.(*factsSystem).cleaned.Store(true)
	a.mu.Lock()
	a.lastFacts = time.Now().Add(-2 * time.Minute)
	a.mu.Unlock()
	if err := a.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ci := <-checkins; ci.Facts == nil || !ci.Facts.PreflightOK {
		t.Fatalf("periodic checkin did not refresh preflight: %+v", ci.Facts)
	}
}
