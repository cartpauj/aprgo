//go:build linux

// TTY raw-mode setup. The Linux tty layer defaults to cooked mode, which would
// eat KISS escape bytes and mangle binary data. We have to put the device into
// raw mode before reading or writing.
package rf

import (
	"os"

	"golang.org/x/sys/unix"
)

// SetRaw configures fd as a raw 8N1 tty: no ICRNL/IXON/echo/canonical, no
// output processing. Safe for /dev/rfcomm0 and real serial ports.
//
// baud=0 leaves the existing speed untouched (sensible for /dev/rfcommN where
// baudrate is irrelevant — RFCOMM is packet-framed by Bluetooth). For real
// serial ports, pass the negotiated rate (1200, 9600, 19200, ...).
func SetRaw(f *os.File, baud int) error {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	t.Iflag &^= unix.ICRNL | unix.IXON | unix.IXOFF | unix.IGNCR | unix.INLCR |
		unix.ISTRIP | unix.IXANY | unix.IMAXBEL | unix.BRKINT | unix.PARMRK | unix.INPCK
	t.Oflag &^= unix.OPOST | unix.ONLCR | unix.OCRNL | unix.ONOCR | unix.ONLRET
	t.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL | unix.ICANON |
		unix.ISIG | unix.IEXTEN | unix.TOSTOP
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8 | unix.CLOCAL | unix.CREAD
	if b, ok := baudConst(baud); ok {
		t.Cflag &^= unix.CBAUD
		t.Cflag |= b
		t.Ispeed = b
		t.Ospeed = b
	}
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}

func baudConst(b int) (uint32, bool) {
	switch b {
	case 1200:
		return unix.B1200, true
	case 2400:
		return unix.B2400, true
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
	case 230400:
		return unix.B230400, true
	}
	return 0, false
}
