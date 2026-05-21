// aprgo is a self-contained APRS iGate, digipeater, and operator console.
//
// One binary: KISS RF over serial/Bluetooth/TCP, APRS-IS client with filter,
// gating, beacons, and a web UI driven entirely by interactive wizards.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"aprgo/internal/igate"
	"aprgo/internal/server"
)

// Version is set via -ldflags="-X main.Version=…" at build time.
var Version = "1.0.0"

func main() {
	listen := flag.String("listen", ":14439", "HTTP listen address")
	statePath := flag.String("state", "/var/lib/aprgo/state.json", "Path to state JSON file")
	dbPath := flag.String("db", "/var/lib/aprgo/db.sqlite", "Path to SQLite database")
	showVer := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVer {
		fmt.Println("aprgo", Version)
		return
	}

	// If launched under systemd, journald adds its own timestamp; suppress ours.
	if os.Getenv("JOURNAL_STREAM") != "" {
		log.SetFlags(0)
	} else {
		log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	}
	igate.SetVersion(Version)
	server.SetVersion(Version)
	log.Printf("aprgo %s starting (listen=%s state=%s db=%s)", Version, *listen, *statePath, *dbPath)

	srv, err := server.New(server.Options{
		Listen:    *listen,
		StatePath: *statePath,
		DBPath:    *dbPath,
	})
	if err != nil {
		log.Fatalf("server init: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
	log.Printf("aprgo: clean shutdown")
	os.Exit(0)
}
