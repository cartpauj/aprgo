// Package igate is aprgo's APRS-IS client.
//
// It connects to a server defined in state.Store, sends a login line with the
// configured filter, reads incoming TNC2 lines and republishes them as
// ax25.Frame{Origin: SrcIS} on the bus, and exposes Send(tnc2) for outgoing
// traffic.
//
// Subscribes to state changes; if the server / passcode / filter / callsign
// mutates, the current connection is dropped and a fresh one dialed.
package igate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"aprgo/internal/ax25"
	"aprgo/internal/bus"
	"aprgo/internal/state"
)

// Verification is the post-login passcode-validation state reported by the
// APRS-IS server. Until we see a `# logresp` line we don't know yet.
type Verification uint8

const (
	VerificationUnknown    Verification = iota
	VerificationVerified                // passcode accepted; uploads will propagate
	VerificationUnverified              // passcode rejected or -1; uploads get qAX'd, dropped by server
)

// Version of aprgo to advertise in the APRS-IS login banner. Overridden via
// igate.SetVersion(...) by main.
var versionString = "dev"

// SetVersion lets cmd/aprgo wire the build-time Version into the login line.
func SetVersion(v string) {
	if v != "" {
		versionString = v
	}
}

// Client is an APRS-IS client. Auto-reconnects.
type Client struct {
	st  *state.Store
	bus *bus.Bus

	mu           sync.Mutex
	conn         net.Conn
	writer       *bufio.Writer
	connected    bool
	verification Verification
	lastError    string
	lastErrorAt  time.Time

	// outQ is the non-blocking send queue. The Send() method enqueues; an
	// internal goroutine bound to each session drains it onto the socket.
	// Bounded so a wedged IS server cannot grow memory without limit.
	outQ chan []byte

	// cancel of current session, set when a session is running so we can drop
	// the connection on state change.
	sessionCancel context.CancelFunc
}

const outQueueDepth = 256

func New(st *state.Store, b *bus.Bus) *Client {
	return &Client{st: st, bus: b, outQ: make(chan []byte, outQueueDepth)}
}

// Verification reports the verified/unverified status from the IS server.
func (c *Client) Verification() Verification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.verification
}

// LastError returns the most recent session error, if any.
func (c *Client) LastError() (string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastError, c.lastErrorAt
}

// Connected reports whether we currently have a live IS session.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Run keeps a connection open, reconnecting on failure, until ctx is cancelled.
// Reacts to state.Subscribe() events by dropping the current session.
func (c *Client) Run(ctx context.Context) {
	stateCh, cancelSub := c.st.Subscribe()
	defer cancelSub()

	go func() {
		for snap := range stateCh {
			_ = snap
			c.dropSession()
		}
	}()

	// Reconnect backoff starts at 5s (per APRS-IS community practice — 1s
	// hammers flapping servers) and caps at 60s.
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		snap := c.st.Snapshot()
		if snap.Callsign == "" || snap.Passcode == "" || snap.ISServer == "" {
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		if err := c.session(ctx, snap); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("igate: session: %v (retry in %s)", err, backoff)
			c.mu.Lock()
			c.lastError = err.Error()
			c.lastErrorAt = time.Now()
			c.mu.Unlock()
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Second
	}
}

func (c *Client) dropSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionCancel != nil {
		c.sessionCancel()
		c.sessionCancel = nil
	}
}

func (c *Client) session(parent context.Context, snap state.State) error {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.sessionCancel = cancel
	c.mu.Unlock()
	defer cancel()

	server := snap.ISServer
	if !strings.Contains(server, ":") {
		// Accept bare hostnames in settings — APRS-IS default filtered-feed port.
		server += ":14580"
	}

	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", server)
	if err != nil {
		return err
	}
	defer conn.Close()

	w := bufio.NewWriter(conn)
	login := fmt.Sprintf("user %s pass %s vers aprgo %s", snap.Callsign, snap.Passcode, versionString)
	if snap.ISFilter != "" {
		login += " filter " + snap.ISFilter
	}
	login += "\r\n"
	if _, err := w.WriteString(login); err != nil {
		return fmt.Errorf("login write: %w", err)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.writer = w
	c.connected = true
	c.verification = VerificationUnknown
	c.lastError = ""
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.writer = nil
		c.connected = false
		c.mu.Unlock()
	}()

	log.Printf("igate: connected %s as %s", snap.ISServer, snap.Callsign)

	// Writer goroutine: drains outQ into the socket. Exits when ctx cancels
	// (session end) or any write errors.
	writeErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				writeErr <- ctx.Err()
				return
			case msg, ok := <-c.outQ:
				if !ok {
					writeErr <- nil
					return
				}
				if _, err := w.Write(msg); err != nil {
					writeErr <- err
					return
				}
				if err := w.Flush(); err != nil {
					writeErr <- err
					return
				}
			}
		}
	}()

	r := bufio.NewReader(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			cancel()
			<-writeErr
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// Server status line — surface verification state and log auth failures.
			if strings.Contains(line, "logresp") {
				v := VerificationUnknown
				if strings.Contains(line, " verified") && !strings.Contains(line, "unverified") {
					v = VerificationVerified
				} else if strings.Contains(line, "unverified") {
					v = VerificationUnverified
					log.Printf("igate: passcode rejected by %s — uploads will be qAX'd: %s",
						server, strings.TrimPrefix(line, "# "))
				}
				c.mu.Lock()
				c.verification = v
				c.mu.Unlock()
			}
			continue
		}
		if frame, ok := parseTNC2(line, server); ok {
			c.bus.Frames.Publish(frame)
		}
	}
}

// parseTNC2 parses "SRC>DEST,P1,P2:info" into an ax25.Frame. Returns false
// if the line doesn't fit the format (e.g. server status lines).
func parseTNC2(line, iface string) (ax25.Frame, bool) {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return ax25.Frame{}, false
	}
	header := line[:colon]
	info := line[colon+1:]
	gt := strings.IndexByte(header, '>')
	if gt < 0 {
		return ax25.Frame{}, false
	}
	src := header[:gt]
	rest := header[gt+1:]
	parts := strings.Split(rest, ",")
	dest := parts[0]
	var path []string
	if len(parts) > 1 {
		path = parts[1:]
	}
	return ax25.Frame{
		Src:    src,
		Dest:   dest,
		Path:   path,
		Info:   []byte(info),
		Raw:    nil, // no AX.25 wire bytes for IS-origin frames
		RxAt:   time.Now(),
		Origin: ax25.SrcIS,
		IFace:  iface,
	}, true
}

// Send queues a TNC2 packet for transmission to APRS-IS. Non-blocking; if the
// outbound queue is saturated (256 deep) the packet is dropped and an error
// returned. While disconnected, packets continue to queue and are flushed by
// the writer goroutine once a session reconnects.
func (c *Client) Send(packet string) error {
	if !strings.HasSuffix(packet, "\r\n") {
		packet += "\r\n"
	}
	b := []byte(packet)
	select {
	case c.outQ <- b:
		return nil
	default:
		return errors.New("igate: send queue full")
	}
}

