package oobd

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// A camera pointed at the test Mac sees what software can't capture: a black
// screen, the boot logo, recovery, a frozen frame, a closed lid. A separate
// capture process (a LaunchAgent running ffmpeg, which macOS's camera
// permission requires) keeps the newest frame in cfg.Camera; oobd serves it.

// cameraStale is how old the newest frame may be before the camera counts as down.
const cameraStale = 10 * time.Second

func (s *Server) frame() ([]byte, time.Time, error) {
	if s.cfg.Camera == "" {
		return nil, time.Time{}, fmt.Errorf("no camera configured on this controller")
	}
	st, err := os.Stat(s.cfg.Camera)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("no camera frame yet (%s)", s.cfg.Camera)
	}
	if age := time.Since(st.ModTime()); age > cameraStale {
		return nil, st.ModTime(), fmt.Errorf("camera frame is %s old: the capture process is not running, or lost camera permission", age.Round(time.Second))
	}
	b, err := os.ReadFile(s.cfg.Camera)
	if err != nil || len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 || b[len(b)-2] != 0xff || b[len(b)-1] != 0xd9 {
		// Caught mid-write: the previous frame will do.
		s.mu.Lock()
		b = s.lastFrame
		s.mu.Unlock()
		if b == nil {
			return nil, st.ModTime(), fmt.Errorf("camera frame unreadable")
		}
	} else {
		s.mu.Lock()
		s.lastFrame = b
		s.mu.Unlock()
	}
	return b, st.ModTime(), nil
}

func (s *Server) hCamera(w http.ResponseWriter, r *http.Request) {
	b, at, err := s.frame()
	if err != nil {
		code := http.StatusServiceUnavailable
		if s.cfg.Camera == "" {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Time", at.UTC().Format(time.RFC3339Nano))
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Write(b)
}

// hCameraStream is MJPEG (multipart/x-mixed-replace), which an <img> plays live.
func (s *Server) hCameraStream(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.frame(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	const boundary = "maclabframe"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-store")
	fl, _ := w.(http.Flusher)
	var last time.Time
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
		b, at, err := s.frame()
		if err != nil || !at.After(last) {
			continue
		}
		last = at
		if _, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(b)); err != nil {
			return
		}
		w.Write(b)
		w.Write([]byte("\r\n"))
		if fl != nil {
			fl.Flush()
		}
	}
}
