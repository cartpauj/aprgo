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

	"aprgo/internal/config"
	"aprgo/internal/igate"
	"aprgo/internal/server"
)

// Version is set via -ldflags="-X main.Version=v1.2.3" at build time by
// the CI release workflow (.github/workflows/release.yml). The "dev"
// default only surfaces for un-released local builds — `go run`, plain
// `go build` without ldflags, etc.
var Version = "dev"

func main() {
	// Default ports: 14473 (HTTP) is a digit-shuffle of 14439 that ends in
	// 443, the universal HTTPS hint — it's the "redirect to TLS" port.
	// 14439 (HTTPS) is the play on 144.390 MHz, the North-American APRS
	// calling frequency — the operator-facing console lives here.
	listenHTTP := flag.String("listen-http", ":14473", "HTTP listen address (redirects to HTTPS for everything except healthz/readyz)")
	listenHTTPS := flag.String("listen-https", ":14439", "HTTPS listen address (self-signed cert)")
	statePath := flag.String("state", "/var/lib/aprgo/state.json", "Path to state JSON file")
	configPath := flag.String("config", "/var/lib/aprgo/aprgo.conf", "Path to config file (credentials + lockdown)")
	dbPath := flag.String("db", "/var/lib/aprgo/db.sqlite", "Path to SQLite database")
	tlsDir := flag.String("tls-dir", "/var/lib/aprgo/tls", "Directory holding the self-signed cert+key")
	regenTLS := flag.Bool("regen-tls", false, "Wipe and regenerate the self-signed cert on startup")
	setPassword := flag.String("set-password", "", "Set the admin password directly in aprgo.conf and exit. Use to recover from a UI lockout — restart aprgo afterwards. Tip: prefix the command with a space so it doesn't land in shell history.")
	showVer := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVer {
		fmt.Println("aprgo", Version)
		return
	}

	if *setPassword != "" {
		cfg, err := config.Open(*configPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			os.Exit(1)
		}
		if err := cfg.SetPassword(*setPassword); err != nil {
			fmt.Fprintln(os.Stderr, "set password:", err)
			os.Exit(1)
		}
		fmt.Printf("password updated in %s\n", *configPath)
		fmt.Println()
		fmt.Println("Now restart aprgo for the change to take effect:")
		fmt.Println("    sudo systemctl restart aprgo")
		return
	}

	// If launched under systemd, journald adds its own timestamp; suppress ours.
	if os.Getenv("JOURNAL_STREAM") != "" {
		log.SetFlags(0)
	} else {
		log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	}
	// Tee log output to an in-memory ring buffer the diagnostics page can
	// surface. journald (or whatever was writing to stderr) still gets
	// everything via the MultiWriter; nothing about external logging
	// changes.
	logBuf := server.NewLogBuffer(200)
	log.SetOutput(logBuf.Tee(os.Stderr))
	igate.SetVersion(Version)
	server.SetVersion(Version)
	log.Printf("aprgo %s starting (http=%s https=%s state=%s config=%s db=%s)",
		Version, *listenHTTP, *listenHTTPS, *statePath, *configPath, *dbPath)

	srv, err := server.New(server.Options{
		ListenHTTP:  *listenHTTP,
		ListenHTTPS: *listenHTTPS,
		StatePath:   *statePath,
		ConfigPath:  *configPath,
		DBPath:      *dbPath,
		TLSDir:      *tlsDir,
		RegenTLS:    *regenTLS,
		LogBuffer:   logBuf,
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
