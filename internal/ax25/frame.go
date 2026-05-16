package ax25

import (
	"strings"
	"time"
)

// Source indicates where a Frame entered the system.
type Source uint8

const (
	SrcRF Source = iota // received over the air
	SrcIS               // received from APRS-IS
	SrcTX               // synthesized to represent something WE transmitted to RF
)

func (s Source) String() string {
	switch s {
	case SrcRF:
		return "RF"
	case SrcIS:
		return "IS"
	case SrcTX:
		return "TX"
	}
	return "?"
}

// Frame is a decoded AX.25 UI frame plus the raw bytes that produced it.
// It is the canonical wire-level event flowing through the bus.
type Frame struct {
	Src     string    // "N0CALL-10"
	Dest    string    // tocall, e.g. "APRGO"
	Path    []string  // each may end "*" if H-bit set
	Info    []byte    // raw info field (may be binary, e.g. Mic-E)
	Raw     []byte    // AX.25 bytes (no KISS, no FCS) for re-TX or hashing
	RxAt    time.Time // when we received it
	Origin  Source    // RF or IS
	IFace   string    // device path, BT addr, or APRSIS server
}

// TNC2 returns the canonical "SRC>DEST,PATH:info" string form of this frame.
// Info bytes are emitted as-is (callers wanting safe display must escape).
func (f Frame) TNC2() string {
	var b strings.Builder
	b.Grow(len(f.Src) + len(f.Dest) + len(f.Info) + 16)
	b.WriteString(f.Src)
	b.WriteByte('>')
	b.WriteString(f.Dest)
	for _, p := range f.Path {
		b.WriteByte(',')
		b.WriteString(p)
	}
	b.WriteByte(':')
	b.Write(f.Info)
	return b.String()
}

// FromAX25 decodes raw AX.25 bytes into a Frame.
func FromAX25(b []byte, origin Source, iface string) (Frame, error) {
	src, dest, digis, info, err := DecodeUIFrame(b)
	if err != nil {
		return Frame{}, err
	}
	return Frame{
		Src:    src,
		Dest:   dest,
		Path:   digis,
		Info:   info,
		Raw:    b,
		RxAt:   time.Now(),
		Origin: origin,
		IFace:  iface,
	}, nil
}
