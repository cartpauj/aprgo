// Package tnc handles discovery, pairing and binding of TNC sources surfaced
// to the user in the web UI:
//
//   - Local serial devices (USB / Bluetooth-bound TTYs)
//   - Bluetooth devices via bluetoothctl (scan / pair / trust)
//   - TCP KISS endpoints (no discovery, user types host:port)
//
// All Bluetooth actions are shelled out to the `bluetoothctl` and `rfcomm`
// CLIs from the `bluez` and `bluez-utils` packages. This trades a thin shell
// dependency for not having to embed BlueZ D-Bus bindings.
package tnc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Serial describes a serial-style TTY device available on the system.
type Serial struct {
	Path  string // e.g. /dev/rfcomm0, /dev/ttyUSB0
	Label string // friendly name for display ("USB Serial", "Bluetooth RFCOMM 0", etc.)
	Kind  string // "rfcomm" | "usb-serial" | "acm" | "serial"
}

// BTDevice describes a Bluetooth device known to BlueZ.
type BTDevice struct {
	Address   string // "AA:BB:CC:DD:EE:FF"
	Name      string // friendly name
	Paired    bool
	Trusted   bool
	Connected bool
}

// ListSerial enumerates likely-TNC TTYs by scanning /dev.
// We don't ssync-load all /sys metadata; just match well-known device
// patterns so the UI gets a quick list. The user picks one and aprgo
// tries to open it.
func ListSerial() ([]Serial, error) {
	patterns := []struct {
		glob  string
		kind  string
		label string
	}{
		{"/dev/rfcomm*", "rfcomm", "Bluetooth RFCOMM"},
		{"/dev/ttyUSB*", "usb-serial", "USB serial"},
		{"/dev/ttyACM*", "acm", "USB CDC-ACM"},
		{"/dev/ttyAMA*", "serial", "Built-in serial"},
		{"/dev/ttyS*", "serial", "Hardware serial"},
	}
	var out []Serial
	for _, p := range patterns {
		matches, _ := filepath.Glob(p.glob)
		sort.Strings(matches)
		for _, m := range matches {
			out = append(out, Serial{Path: m, Label: p.label + " (" + filepath.Base(m) + ")", Kind: p.kind})
		}
	}
	return out, nil
}

// Paired returns the currently-paired Bluetooth devices known to BlueZ.
func Paired(ctx context.Context) ([]BTDevice, error) {
	out, err := run(ctx, 5*time.Second, "bluetoothctl", "devices", "Paired")
	if err != nil {
		// Some bluetoothctl versions don't support the "Paired" filter; fall back.
		out, err = run(ctx, 5*time.Second, "bluetoothctl", "paired-devices")
		if err != nil {
			return nil, err
		}
	}
	devs := parseDeviceList(out)
	for i := range devs {
		devs[i].Paired = true
		// Enrich with info if cheap
		if info, err := run(ctx, 3*time.Second, "bluetoothctl", "info", devs[i].Address); err == nil {
			devs[i].Trusted = strings.Contains(info, "Trusted: yes")
			devs[i].Connected = strings.Contains(info, "Connected: yes")
		}
	}
	return devs, nil
}

// Scan runs a discoverable scan for `duration` and returns the devices found.
// Re-runs bluetoothctl in non-interactive mode.
func Scan(ctx context.Context, duration time.Duration) ([]BTDevice, error) {
	if err := EnsureBTReady(ctx); err != nil {
		return nil, err
	}
	if duration <= 0 {
		duration = 8 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, duration+5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "bluetoothctl", "--timeout", fmt.Sprintf("%d", int(duration.Seconds())), "scan", "on")
	if _, err := cmd.CombinedOutput(); err != nil {
		// Scan errors are common (Already scanning, etc.); continue and list devices.
	}
	out, err := run(ctx, 5*time.Second, "bluetoothctl", "devices")
	if err != nil {
		return nil, err
	}
	return parseDeviceList(out), nil
}

// EnsureBTReady makes sure the Bluetooth adapter is ready for scan/pair:
//   - Unblocks the controller if it's rfkill-soft-blocked (we run as root,
//     and the operator can't always run rfkill themselves).
//   - Powers on the controller via bluetoothctl if it's off.
//
// Returns a clear, actionable error if the adapter is hard-blocked (physical
// switch — software can't override), if no controller exists at all (no BT
// hardware / driver hasn't bound), or if the bluetooth service isn't
// available. Safe to call repeatedly; cheap when already ready.
func EnsureBTReady(ctx context.Context) error {
	// 1. rfkill: distinguish soft vs hard block. We don't fail outright if
	//    rfkill isn't installed — bluetoothctl power on may still work.
	if _, err := exec.LookPath("rfkill"); err == nil {
		out, _ := run(ctx, 3*time.Second, "rfkill", "list", "bluetooth")
		// rfkill output for each entry:
		//   0: hci0: Bluetooth
		//   	Soft blocked: yes
		//   	Hard blocked: no
		if strings.Contains(out, "Hard blocked: yes") {
			return errors.New("bluetooth is hard-blocked by a physical switch or firmware setting — flip the device's Bluetooth/airplane switch on, then retry")
		}
		if strings.Contains(out, "Soft blocked: yes") {
			if _, err := run(ctx, 3*time.Second, "rfkill", "unblock", "bluetooth"); err != nil {
				return fmt.Errorf("bluetooth is soft-blocked and aprgo couldn't unblock it (rfkill unblock failed: %w) — try `sudo rfkill unblock bluetooth` and retry", err)
			}
		}
	}

	// 2. bluetoothctl power on. If there's no controller at all, this fails
	//    with "No default controller available" — surface that as a clear
	//    hardware/driver error rather than a generic exec failure.
	if _, err := exec.LookPath("bluetoothctl"); err != nil {
		return errors.New("bluetoothctl not found in PATH — install the bluez package so aprgo can manage the BT adapter")
	}
	out, err := run(ctx, 3*time.Second, "bluetoothctl", "power", "on")
	if err != nil {
		if strings.Contains(out, "No default controller") {
			return errors.New("no Bluetooth controller found — check `dmesg | grep -i bluetooth` for driver errors; on a Pi the firmware in /lib/firmware/brcm/ may be missing or the UART for hci0 hasn't attached")
		}
		return fmt.Errorf("bluetoothctl power on: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// Pair attempts to pair + trust a Bluetooth device. Relies on Just-Works
// pairing (no PIN/passkey — almost all APRS TNCs work this way).
//
// BlueZ refuses to let bluetoothctl register a pairing agent in our
// (non-interactive, non-TTY) context, so any `bluetoothctl pair` call we
// make returns success without actually exchanging keys — the device ends
// up Paired:no, then rfcomm connect drops the SPP channel within seconds.
// The fix is to run bt-agent (from bluez-tools) as a separate process
// for the duration of the pair: it registers as the system-wide BlueZ
// pairing agent and auto-confirms Just-Works requests. We start it,
// pair, then stop it.
func Pair(ctx context.Context, addr string) error {
	if !btAddrRE.MatchString(addr) {
		return fmt.Errorf("bad bluetooth address %q", addr)
	}
	if err := EnsureBTReady(ctx); err != nil {
		return err
	}
	// If already paired, BlueZ rejects re-pair attempts. Just (re)trust.
	if info, err := run(ctx, 3*time.Second, "bluetoothctl", "info", addr); err == nil {
		if strings.Contains(info, "Paired: yes") {
			_, _ = run(ctx, 3*time.Second, "bluetoothctl", "trust", addr)
			return nil
		}
	}
	// Spawn bt-agent in the background as the system pairing agent. It
	// auto-confirms incoming Just-Works requests and exits when we send
	// SIGTERM. If it isn't installed, surface a clear error pointing the
	// operator at the required system package.
	if _, err := exec.LookPath("bt-agent"); err != nil {
		return fmt.Errorf("bt-agent not found in PATH — install the bluez-tools package (apt install bluez-tools) so aprgo can run the BlueZ pairing agent during the pair operation")
	}
	agentCtx, cancelAgent := context.WithCancel(ctx)
	defer cancelAgent()
	agent := exec.CommandContext(agentCtx, "bt-agent", "--capability=NoInputNoOutput")
	agent.Stdout = nil
	agent.Stderr = nil
	if err := agent.Start(); err != nil {
		return fmt.Errorf("start bt-agent: %w", err)
	}
	defer func() { _ = agent.Process.Kill(); _ = agent.Wait() }()
	// Give bt-agent a moment to register with BlueZ before we initiate
	// the pair (registration is async on the system D-Bus).
	time.Sleep(500 * time.Millisecond)

	if _, err := run(ctx, 25*time.Second, "bluetoothctl", "pair", addr); err != nil {
		return fmt.Errorf("pair %s: %w", addr, err)
	}
	if _, err := run(ctx, 3*time.Second, "bluetoothctl", "trust", addr); err != nil {
		return fmt.Errorf("trust %s: %w", addr, err)
	}
	// Verify — bluetoothctl can return 0 even when the device declined keys.
	info, err := run(ctx, 3*time.Second, "bluetoothctl", "info", addr)
	if err != nil {
		return fmt.Errorf("post-pair info %s: %w", addr, err)
	}
	if !strings.Contains(info, "Paired: yes") {
		return fmt.Errorf("pair %s reported success but device still shows Paired:no — make sure the TNC is powered on and within range, then try again", addr)
	}
	return nil
}

// Bind creates a /dev/rfcomm<n> device for a paired BT TNC on channel 1.
// Returns the chosen device path. The caller is responsible for persisting
// the (address, dev) mapping so it can be re-bound on next boot.
func Bind(ctx context.Context, addr string, channel int) (string, error) {
	if !btAddrRE.MatchString(addr) {
		return "", fmt.Errorf("bad bluetooth address %q", addr)
	}
	if channel <= 0 {
		channel = 1
	}
	// Pick the lowest free rfcomm index.
	idx := 0
	for {
		if _, err := os.Stat(fmt.Sprintf("/dev/rfcomm%d", idx)); errors.Is(err, os.ErrNotExist) {
			break
		}
		idx++
		if idx > 31 {
			return "", errors.New("no free /dev/rfcommN slot")
		}
	}
	if _, err := run(ctx, 5*time.Second, "rfcomm", "bind", fmt.Sprintf("%d", idx), addr, fmt.Sprintf("%d", channel)); err != nil {
		return "", fmt.Errorf("rfcomm bind: %w", err)
	}
	return fmt.Sprintf("/dev/rfcomm%d", idx), nil
}

// CurrentRfcommMAC queries the kernel for the bluetooth MAC currently bound
// to `dev` (e.g. "/dev/rfcomm0") and returns it as an uppercase string.
//
// Returns ("", nil) if no binding exists OR `dev` isn't an rfcomm device.
// Returns ("", err) only when the `rfcomm` subprocess itself fails to run.
//
// Used by aprgo's self-heal logic: state.TNCAddr is supposed to track the
// MAC of whatever's bound to TNCSerial, but the wizard's serial-picker
// path can clear it. By asking the kernel directly, we recover even when
// state is stale.
func CurrentRfcommMAC(ctx context.Context, dev string) (string, error) {
	idx := strings.TrimPrefix(dev, "/dev/rfcomm")
	if idx == dev || idx == "" {
		return "", nil
	}
	out, err := run(ctx, 3*time.Second, "rfcomm")
	if err != nil {
		return "", err
	}
	// rfcomm with no args prints one line per binding:
	//   rfcomm0: 34:81:F4:6C:5E:26 channel 6 connected [reuse-dlc ...]
	prefix := "rfcomm" + idx + ":"
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		// First whitespace-separated token after the prefix is the MAC.
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return "", nil
		}
		mac := strings.ToUpper(fields[0])
		if btAddrRE.MatchString(mac) {
			return mac, nil
		}
		return "", nil
	}
	return "", nil
}

// Release tears down an rfcomm binding.
func Release(ctx context.Context, dev string) error {
	idx := strings.TrimPrefix(dev, "/dev/rfcomm")
	if idx == dev {
		return fmt.Errorf("not an rfcomm device: %s", dev)
	}
	_, err := run(ctx, 3*time.Second, "rfcomm", "release", idx)
	return err
}

// DiscoverSPPChannel queries the device's SDP record for the Serial Port
// Profile and returns the RFCOMM channel it advertises. Falls back to 1
// only after several retries — BlueZ's SDP cache doesn't populate until a
// brief moment after pair completion (~1–2 s), so the first sdptool query
// often comes back empty and a naive default-to-1 silently breaks devices
// that use any other channel (Mobilinkd TNC3 uses channel 6).
//
// 6 attempts × 1 s spacing = up to ~6 s budget for SDP to populate, on top
// of sdptool's own 10 s socket timeout. In practice the second or third
// retry succeeds.
func DiscoverSPPChannel(ctx context.Context, addr string) int {
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 1
			case <-time.After(time.Second):
			}
		}
		out, err := run(ctx, 10*time.Second, "sdptool", "search", "--bdaddr", addr, "SP")
		if err != nil || out == "" {
			continue
		}
		if m := channelRE.FindStringSubmatch(out); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil && v > 0 && v < 31 {
				return v
			}
		}
	}
	return 1
}

// ChooseRFCOMMFor returns the rfcomm device path to use for the given BT
// address. If `addr` is already bound to a `/dev/rfcommN` slot, that
// existing slot is reused — re-pairing the same TNC shouldn't accumulate
// new device numbers (rfcomm0 then rfcomm1 then rfcomm2...) every attempt.
// Otherwise picks the lowest free slot.
//
// `rfcomm -a` output format:
//
//	rfcommN: <HOST_ADAPTER_MAC> -> <DEVICE_MAC> channel N <state> [flags]
//
// We compare against <DEVICE_MAC> (the TNC's address, parts[3]), not the
// host adapter's MAC (parts[1]).
func ChooseRFCOMMFor(addr string) (string, error) {
	addrUp := strings.ToUpper(addr)
	if out, err := exec.Command("rfcomm", "-a").Output(); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || !strings.HasPrefix(line, "rfcomm") {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) < 4 {
				continue
			}
			name := strings.TrimSuffix(parts[0], ":") // "rfcomm0"
			deviceMAC := strings.ToUpper(parts[3])    // <DEVICE_MAC>
			if deviceMAC == addrUp {
				return "/dev/" + name, nil
			}
		}
	}
	return ChooseFreeRFCOMM()
}

// ErrNoFreeRFCOMM is returned when all 32 BlueZ RFCOMM slots are already
// bound. The operator's recovery is `sudo rfcomm release all` (or release
// a specific N they know they're not using).
var ErrNoFreeRFCOMM = errors.New("no free /dev/rfcommN slot (all 32 bound — try `sudo rfcomm release all`)")

// ChooseFreeRFCOMM returns the lowest /dev/rfcommN slot that isn't already
// bound or present. Returns ErrNoFreeRFCOMM when all 32 are taken — callers
// must NOT fall back to a hardcoded slot, since binding to an already-bound
// index silently kicks off whoever's using it.
func ChooseFreeRFCOMM() (string, error) {
	for i := 0; i < 32; i++ {
		path := fmt.Sprintf("/dev/rfcomm%d", i)
		if _, err := os.Stat(path); err != nil {
			return path, nil
		}
	}
	return "", ErrNoFreeRFCOMM
}

var channelRE = regexp.MustCompile(`(?i)Channel:\s*(\d+)`)

// ---- internals ----

var btAddrRE = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)
var devLineRE = regexp.MustCompile(`^Device ([0-9A-Fa-f:]{17}) (.+)$`)

func parseDeviceList(out string) []BTDevice {
	var devs []BTDevice
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		m := devLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		devs = append(devs, BTDevice{Address: m[1], Name: m[2]})
	}
	return devs
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}
