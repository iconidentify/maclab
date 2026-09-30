// Package oob is labd's side of out-of-band control: hard reset and serial
// console for a Mac, through whatever controller is attached to it.
package oob

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

type Controller interface {
	Name() string
	// Reset hard-reboots the Mac regardless of what its OS is doing.
	Reset(ctx context.Context) error
	// Serial streams console lines to fn until ctx ends or the stream breaks.
	Serial(ctx context.Context, fn func(line string)) error
	Status(ctx context.Context) (string, error)
	// Arm puts the Mac back into serial mode, retrying for window while it reboots.
	Arm(ctx context.Context, window time.Duration) error
	// Raw copies console bytes to w until ctx ends; Write types into the console.
	Raw(ctx context.Context, w io.Writer) error
	Write(ctx context.Context, b []byte) error
}

func New(c *api.OOBConfig) (Controller, error) {
	switch c.Driver {
	case "oobd", "":
		return &OOBD{URL: strings.TrimRight(c.URL, "/"), Token: c.Token}, nil
	}
	return nil, fmt.Errorf("unknown oob driver %q", c.Driver)
}

// OOBD talks to an oobd daemon: a macOS host running macvdmtool, or later a
// Linux host with a Central Scrutinizer. Both expose the same HTTP API.
type OOBD struct {
	URL   string
	Token string
}

func (o *OOBD) Name() string { return "oobd " + o.URL }

func (o *OOBD) do(ctx context.Context, method, path string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, o.URL+path, nil)
	if err != nil {
		return nil, err
	}
	if o.Token != "" {
		req.Header.Set("Authorization", "Bearer "+o.Token)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (o *OOBD) Reset(ctx context.Context) error {
	resp, err := o.do(ctx, "POST", "/v1/reset", time.Minute)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (o *OOBD) Arm(ctx context.Context, window time.Duration) error {
	resp, err := o.do(ctx, "POST", fmt.Sprintf("/v1/serial/arm?for=%d", int(window.Seconds())), time.Minute)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (o *OOBD) Raw(ctx context.Context, w io.Writer) error {
	resp, err := o.do(ctx, "GET", "/v1/serial/raw", 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

func (o *OOBD) Write(ctx context.Context, b []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", o.URL+"/v1/serial/write", bytes.NewReader(b))
	if err != nil {
		return err
	}
	if o.Token != "" {
		req.Header.Set("Authorization", "Bearer "+o.Token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		m, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("console write: %s: %s", resp.Status, strings.TrimSpace(string(m)))
	}
	return nil
}

func (o *OOBD) Status(ctx context.Context) (string, error) {
	resp, err := o.do(ctx, "GET", "/v1/status", 30*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b)), nil
}

func (o *OOBD) Serial(ctx context.Context, fn func(string)) error {
	resp, err := o.do(ctx, "GET", "/v1/serial?follow=1", 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fn(sc.Text())
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}
