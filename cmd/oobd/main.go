// Command oobd runs on the controller host attached to a test Mac's DFU port
// and gives labd hard reset and the serial console.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/iconidentify/maclab/internal/oobd"
)

func main() {
	listen := flag.String("listen", ":7780", "")
	token := flag.String("token", os.Getenv("OOBD_TOKEN"), "bearer token labd must present")
	tool := flag.String("tool", "/usr/local/bin/macvdmtool", "macvdmtool (or a compatible reset tool)")
	serial := flag.String("serial", "/dev/cu.debug-console", "serial device the target's console appears on")
	baud := flag.Int("baud", 115200, "")
	logFile := flag.String("log", "", "also append the console to this file")
	camera := flag.String("camera", "", "JPEG a capture process keeps updating with the newest frame of a camera pointed at the Mac")
	flag.Parse()
	if *token == "" {
		log.Fatal("set --token (or OOBD_TOKEN): anyone who can reach oobd can hard-reset the Mac")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s := oobd.New(oobd.Config{Token: *token, Tool: *tool, SerialDev: *serial, Baud: *baud, LogFile: *logFile, Camera: *camera}, log.Default())
	go s.ReadSerial(ctx)
	hs := &http.Server{Addr: *listen, Handler: s.Handler()}
	go func() { <-ctx.Done(); hs.Close() }()
	log.Printf("oobd on %s: reset via %s, console %s", *listen, *tool, *serial)
	if err := hs.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
