package oobd

import (
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCamera(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 'x', 'y', 0xff, 0xd9}
	path := filepath.Join(t.TempDir(), "latest.jpg")
	get := func(s *Server) (int, string) {
		w := httptest.NewRecorder()
		s.hCamera(w, httptest.NewRequest("GET", "/v1/camera", nil))
		b, _ := io.ReadAll(w.Body)
		return w.Code, string(b)
	}
	quiet := log.New(io.Discard, "", 0)

	if code, _ := get(New(Config{}, quiet)); code != 404 {
		t.Fatalf("no camera configured: %d", code)
	}
	s := New(Config{Camera: path}, quiet)
	if code, _ := get(s); code != 503 {
		t.Fatalf("no frame yet: %d", code)
	}
	os.WriteFile(path, jpeg, 0o644)
	if code, body := get(s); code != 200 || body != string(jpeg) {
		t.Fatalf("fresh frame: %d %q", code, body)
	}
	// Caught mid-write: serve the previous good frame, not a torn one.
	os.WriteFile(path, jpeg[:3], 0o644)
	if code, body := get(s); code != 200 || body != string(jpeg) {
		t.Fatalf("torn frame: %d %q", code, body)
	}
	old := time.Now().Add(-time.Minute)
	os.Chtimes(path, old, old)
	if code, body := get(s); code != 503 || body == "" {
		t.Fatalf("stale frame: %d %q", code, body)
	}
}
