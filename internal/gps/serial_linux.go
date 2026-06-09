//go:build linux

package gps

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openGPSSerial opens a tty read-only and puts it in raw 8N1 mode at baud.
// GPS receivers only emit; we never write, so O_RDONLY keeps us off devices
// we have no business driving. baud=0 leaves the current speed (correct for
// USB CDC-ACM, where the line rate is irrelevant). O_NONBLOCK on open avoids
// hanging on modem-control lines; we clear it immediately so reads block
// normally (a close from another goroutine is what unblocks them).
func openGPSSerial(path string, baud int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := unix.SetNonblock(int(f.Fd()), false); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("clear nonblock %s: %w", path, err)
	}
	if err := setRawGPS(f, baud); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("set raw %s: %w", path, err)
	}
	return f, nil
}

// setRawGPS configures fd as a raw, line-readable 8N1 tty. Mirrors
// rf.SetRaw but read-oriented. ICANON is left off (we frame on \n ourselves
// via bufio.Scanner), echo/signals off, no input translation that would
// mangle NMEA bytes.
func setRawGPS(f *os.File, baud int) error {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	t.Iflag &^= unix.ICRNL | unix.IXON | unix.IXOFF | unix.IGNCR | unix.INLCR |
		unix.ISTRIP | unix.IXANY | unix.IMAXBEL | unix.BRKINT | unix.PARMRK | unix.INPCK
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL | unix.ICANON |
		unix.ISIG | unix.IEXTEN | unix.TOSTOP
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8 | unix.CLOCAL | unix.CREAD
	if b, ok := gpsBaudConst(baud); ok {
		t.Cflag &^= unix.CBAUD
		t.Cflag |= b
		t.Ispeed = b
		t.Ospeed = b
	}
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}

func gpsBaudConst(b int) (uint32, bool) {
	switch b {
	case 4800:
		return unix.B4800, true
	case 9600:
		return unix.B9600, true
	case 19200:
		return unix.B19200, true
	case 38400:
		return unix.B38400, true
	case 57600:
		return unix.B57600, true
	case 115200:
		return unix.B115200, true
	}
	return 0, false
}
