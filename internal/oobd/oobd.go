// Package oobd runs on the controller host physically attached to a test Mac:
// a macOS Mac with macvdmtool today, a Linux box with a Central Scrutinizer later.
// It serves hard reset and the serial console to labd.
package oobd

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Token string
	// Tool is the reset backend command, run as `<Tool> reboot serial` and `<Tool> nop`.
	Tool      string
	SerialDev string
	Baud      int
	LogFile   string
	// Camera is a JPEG a capture process keeps overwriting with the newest frame.
	Camera string
}

type Server struct {
	cfg Config
	log *log.Logger

	toolMu sync.Mutex // macvdmtool must not run concurrently

	mu      sync.Mutex
	ring    []string
	subs    map[chan string]bool
	raw     map[chan []byte]bool
	port    *os.File
	openErr string
	last    time.Time
	typed   time.Time // last write into the console by a person or agent
	rx      time.Time // last byte received from the Mac (not oobd's own markers)

	armUntil  time.Time
	lastFrame []byte
}

func New(cfg Config, logger *log.Logger) *Server {
	return &Server{cfg: cfg, log: logger, subs: map[chan string]bool{}, raw: map[chan []byte]bool{}}
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/status", s.auth(s.hStatus))
	m.HandleFunc("GET /v1/camera", s.auth(s.hCamera))
	m.HandleFunc("GET /v1/camera/stream", s.auth(s.hCameraStream))
	m.HandleFunc("POST /v1/reset", s.auth(s.hReset))
	m.HandleFunc("POST /v1/serial/arm", s.auth(s.hArm))
	m.HandleFunc("GET /v1/serial", s.auth(s.hSerial))
	m.HandleFunc("GET /v1/serial/raw", s.auth(s.hRaw))
	m.HandleFunc("POST /v1/serial/write", s.auth(s.hWrite))
	return m
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			http.Error(w, "bad token", 401)
			return
		}
		h(w, r)
	}
}

func (s *Server) tool(ctx context.Context, args ...string) (string, error) {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.cfg.Tool, args...).CombinedOutput()
	s.log.Printf("%s %s: err=%v\n%s", s.cfg.Tool, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", s.cfg.Tool, strings.Join(args, " "), err, lastLine(string(out)))
	}
	return string(out), nil
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return l[len(l)-1]
}

func (s *Server) hStatus(w http.ResponseWriter, r *http.Request) {
	out, err := s.tool(r.Context(), "nop")
	s.mu.Lock()
	serial := "serial: " + s.cfg.SerialDev
	if s.openErr != "" {
		serial += " (" + s.openErr + ")"
	} else if !s.last.IsZero() {
		serial += fmt.Sprintf(" (last line %s ago)", time.Since(s.last).Round(time.Second))
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error()+"\n"+serial, 502)
		return
	}
	fmt.Fprintf(w, "%s\n%s\n", strings.TrimSpace(out), serial)
}

// hReset hard-reboots the target and puts it straight back into serial mode.
func (s *Server) hReset(w http.ResponseWriter, r *http.Request) {
	s.emit("[oobd] hard reset requested")
	// Reboot on its own: `reboot serial` also switches serial mode on after
	// the reset, and that second step can fail against a Mac that is already
	// going down, reporting a reset that happened as failed (labd then
	// resets it again). Serial mode is restored by the arm loop instead.
	if _, err := s.tool(r.Context(), "reboot"); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	s.startArm(3 * time.Minute)
	w.Write([]byte("ok\n"))
}

// startArm keeps re-arming serial mode for up to d while the console is silent.
func (s *Server) startArm(d time.Duration) {
	s.mu.Lock()
	until := time.Now().Add(d)
	running := !s.armUntil.IsZero()
	if until.After(s.armUntil) {
		s.armUntil = until
	}
	s.mu.Unlock()
	if !running {
		go s.armLoop()
	}
}

// hArm puts the target into serial mode. With ?for=N it keeps re-arming for
// up to N seconds while the console is silent: the Mac drops out of serial
// mode on every reboot, and comes back at an unknown moment.
func (s *Server) hArm(w http.ResponseWriter, r *http.Request) {
	secs, _ := strconv.Atoi(r.URL.Query().Get("for"))
	if secs <= 0 {
		if _, err := s.tool(r.Context(), "serial"); err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		w.Write([]byte("ok\n"))
		return
	}
	s.startArm(time.Duration(min(secs, 600)) * time.Second)
	w.WriteHeader(202)
	w.Write([]byte("arming\n"))
}

func (s *Server) armLoop() {
	start := time.Now()
	var armed time.Time // our last re-arm, made while the console was silent
	defer func() {
		s.mu.Lock()
		s.armUntil = time.Time{}
		s.mu.Unlock()
	}()
	for {
		s.mu.Lock()
		until, rx, typed := s.armUntil, s.rx, s.typed
		s.mu.Unlock()
		if time.Now().After(until) {
			return
		}
		// Every re-arm puts a stray byte on the line, and whatever is reading
		// the console takes it as a keypress: a shell gets garbage, GRUB's
		// menu stops its countdown and waits forever, U-Boot aborts autoboot.
		// So stop the moment serial mode is proven back.
		//
		// Proof 1: the Mac sent output after one of our re-arms.
		if !armed.IsZero() && rx.After(armed) {
			s.log.Printf("serial: output after re-arm, stopping re-arm")
			return
		}
		// Proof 2: someone is typing and the Mac echoes it back.
		if time.Since(typed) < 30*time.Second && rx.After(typed) {
			s.log.Printf("serial: console is echoing typed input, stopping re-arm")
			return
		}
		flowing := time.Since(rx) < 3*time.Second
		if flowing && time.Since(start) > 30*time.Second {
			s.log.Printf("serial: flowing again after %s, stopping re-arm", time.Since(start).Round(time.Second))
			return
		}
		if !flowing {
			if _, err := s.tool(context.Background(), "serial"); err == nil {
				armed = time.Now()
				s.emit("[oobd] serial re-armed")
			}
		}
		time.Sleep(4 * time.Second)
	}
}

// hSerial sends the buffered console, then (with follow=1) streams new lines.
func (s *Server) hSerial(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fl, _ := w.(http.Flusher)
	ch := make(chan string, 1000)
	s.mu.Lock()
	follow := r.URL.Query().Get("follow") == "1"
	if follow {
		s.subs[ch] = true
	} else {
		for _, l := range s.ring {
			fmt.Fprintln(w, l)
		}
	}
	s.mu.Unlock()
	if !follow {
		return
	}
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	if fl != nil {
		fl.Flush()
	}
	keep := time.NewTicker(30 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case l := <-ch:
			fmt.Fprintln(w, l)
		case <-keep.C:
			fmt.Fprintln(w, "") // keeps idle connections from being reaped
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

func (s *Server) emit(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = time.Now()
	s.ring = append(s.ring, line)
	if len(s.ring) > 5000 {
		s.ring = s.ring[len(s.ring)-5000:]
	}
	for ch := range s.subs {
		select {
		case ch <- line:
		default: // a slow reader loses lines rather than stalling the console
		}
	}
	if s.cfg.LogFile != "" {
		if f, err := os.OpenFile(s.cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02T15:04:05.000"), line)
			f.Close()
		}
	}
}

// ReadSerial keeps the serial device open, reopening it whenever it goes away.
// ReadSerial keeps the serial device open, reopening it whenever it goes away.
// Raw bytes go to console clients as they arrive; complete lines go to the log.
func (s *Server) ReadSerial(ctx context.Context) {
	for ctx.Err() == nil {
		f, err := openSerial(s.cfg.SerialDev, s.cfg.Baud)
		if err != nil {
			s.mu.Lock()
			changed := s.openErr != err.Error()
			s.openErr = err.Error()
			s.mu.Unlock()
			if changed {
				s.log.Printf("serial: %v (retrying)", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		s.mu.Lock()
		s.openErr = ""
		s.port = f
		s.mu.Unlock()
		s.log.Printf("serial: reading %s", s.cfg.SerialDev)
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
			case <-stop:
			}
			f.Close()
		}()
		var partial []byte
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				s.emitRaw(chunk)
				partial = append(partial, chunk...)
				for {
					i := bytesIndex(partial, '\n')
					if i < 0 {
						break
					}
					if l := strings.TrimRight(string(partial[:i]), "\r"); l != "" {
						s.emit(l)
					}
					partial = partial[i+1:]
				}
				if len(partial) > 8192 {
					s.emit(string(partial))
					partial = nil
				}
			}
			if err != nil {
				s.log.Printf("serial: %s closed: %v", s.cfg.SerialDev, err)
				break
			}
		}
		close(stop)
		s.mu.Lock()
		s.port = nil
		s.mu.Unlock()
		time.Sleep(time.Second)
	}
}

func bytesIndex(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func (s *Server) emitRaw(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = time.Now()
	s.rx = s.last
	for ch := range s.raw {
		select {
		case ch <- b:
		default:
		}
	}
}

// hRaw streams the console byte for byte, for interactive shells.
func (s *Server) hRaw(w http.ResponseWriter, r *http.Request) {
	fl, _ := w.(http.Flusher)
	ch := make(chan []byte, 1024)
	s.mu.Lock()
	s.raw[ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.raw, ch)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(200)
	if fl != nil {
		fl.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			if _, err := w.Write(b); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

// hWrite types bytes into the target's console.
func (s *Server) hWrite(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	f := s.port
	s.mu.Unlock()
	if f == nil {
		http.Error(w, "serial port is not open", 503)
		return
	}
	if _, err := f.Write(b); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	s.mu.Lock()
	s.typed = time.Now()
	s.mu.Unlock()
	w.WriteHeader(204)
}
