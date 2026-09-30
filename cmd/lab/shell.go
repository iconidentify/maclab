package main

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/term"

	"github.com/iconidentify/maclab/internal/client"
)

// shell attaches the terminal to a Mac's serial console through its
// out-of-band controller. It needs no network on the Mac: bytes travel over
// the USB-C debug link to the controller Mac, then to labd.
func shell(ctx context.Context, c *client.Client, name string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("lab shell needs a terminal")
	}
	if err := c.ConsoleArm(ctx, name); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fmt.Fprintf(os.Stderr, "Connected to %s's serial console. Ctrl-] to leave.\r\n", name)
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), old)

	errc := make(chan error, 2)
	go func() { errc <- c.ConsoleRaw(ctx, name, os.Stdout) }()
	go func() {
		c.ConsoleWrite(ctx, name, []byte("\r")) // wake the prompt
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				errc <- err
				return
			}
			for i := 0; i < n; i++ {
				if buf[i] == 0x1d { // Ctrl-]
					if i > 0 {
						c.ConsoleWrite(ctx, name, buf[:i])
					}
					errc <- nil
					return
				}
			}
			if err := c.ConsoleWrite(ctx, name, buf[:n]); err != nil {
				errc <- err
				return
			}
		}
	}()
	err = <-errc
	term.Restore(int(os.Stdin.Fd()), old)
	fmt.Fprintf(os.Stderr, "\r\nDisconnected from %s.\n", name)
	return err
}
