// Package ax25 contains AX.25 UI frame encode/decode and KISS framing.
package ax25

const (
	FEND  = 0xC0
	FESC  = 0xDB
	TFEND = 0xDC
	TFESC = 0xDD
)

// KISS command bytes (port 0 high-nibble). Per KA9Q 1987 KISS spec.
const (
	KISSCmdData      = 0x00
	KISSCmdTXDelay   = 0x01 // value: TX delay in 10ms units
	KISSCmdPersist   = 0x02 // value: 0-255 p-persistence
	KISSCmdSlotTime  = 0x03 // value: slot time in 10ms units
	KISSCmdTXTail    = 0x04 // value: TX tail in 10ms units (deprecated)
	KISSCmdFullDup   = 0x05
	KISSCmdHardware  = 0x06
)

// EncodeKISSParam builds a single-byte KISS configuration frame on port 0.
// `cmd` is one of the KISSCmd* constants (excluding KISSCmdData).
func EncodeKISSParam(cmd, value byte) []byte {
	out := []byte{FEND, cmd, value}
	// The value byte itself can be 0xC0/0xDB — escape if so.
	if value == FEND {
		out = []byte{FEND, cmd, FESC, TFEND, FEND}
		return out
	}
	if value == FESC {
		out = []byte{FEND, cmd, FESC, TFESC, FEND}
		return out
	}
	return append(out, FEND)
}

// EncodeKISS wraps payload as a KISS data frame on port 0 (cmd byte 0x00).
func EncodeKISS(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+8)
	out = append(out, FEND, 0x00)
	for _, b := range payload {
		switch b {
		case FEND:
			out = append(out, FESC, TFEND)
		case FESC:
			out = append(out, FESC, TFESC)
		default:
			out = append(out, b)
		}
	}
	out = append(out, FEND)
	return out
}

// DecodeKISS unescapes a single inter-FEND segment and strips the type byte.
// Returns nil if the segment is not a port-0 data frame.
func DecodeKISS(seg []byte) []byte {
	if len(seg) < 2 {
		return nil
	}
	if seg[0]&0x0F != 0x00 {
		return nil // not a data frame
	}
	out := make([]byte, 0, len(seg))
	i := 1
	for i < len(seg) {
		if seg[i] == FESC {
			// Per the KISS spec: "Receipt of any character other than
			// TFESC or TFEND while in escaped mode is an error; no action
			// is taken and frame assembly continues." So a FESC followed
			// by an invalid byte drops both (no emit); a trailing lone
			// FESC also drops. Previously we kept the byte-after as data
			// and let trailing-FESC fall through as a literal 0xDB, both
			// of which silently corrupted downstream AX.25 parse.
			if i+1 >= len(seg) {
				break
			}
			switch seg[i+1] {
			case TFEND:
				out = append(out, FEND)
			case TFESC:
				out = append(out, FESC)
			}
			i += 2
			continue
		}
		out = append(out, seg[i])
		i++
	}
	return out
}

// KISSFrameSplitter is a stateful streaming splitter that yields one
// inter-FEND segment per Push() call's append. Use it on raw bytes read from
// the TNC.
type KISSFrameSplitter struct {
	buf []byte
}

// MaxFrameBytes caps the splitter buffer. AX.25 UI frames are at most ~330
// bytes; with KISS escape doubling the worst case is ~700. We use 4 KiB as a
// generous upper bound. A garbage stream with no FEND for this many bytes is
// dropped (resets the splitter).
const MaxFrameBytes = 4096

// Push feeds raw bytes and returns any complete frames (between FEND bytes).
// Empty/run-of-FEND segments are skipped. If the buffer grows past
// MaxFrameBytes without seeing a FEND, the buffer is reset (data discarded).
func (k *KISSFrameSplitter) Push(b []byte) [][]byte {
	k.buf = append(k.buf, b...)
	var frames [][]byte
	for {
		i := bytesIndexByte(k.buf, FEND)
		if i < 0 {
			// No frame boundary yet — guard against unbounded growth.
			if len(k.buf) > MaxFrameBytes {
				k.buf = k.buf[:0]
			}
			return frames
		}
		if i > 0 {
			frame := make([]byte, i)
			copy(frame, k.buf[:i])
			frames = append(frames, frame)
		}
		k.buf = k.buf[i+1:]
		// Periodically reclaim the backing array. Repeated `k.buf[i+1:]`
		// only slides the slice header; the underlying array peak-grows
		// to MaxFrameBytes and stays there. When the slice has been slid
		// a lot relative to its remaining content, copy down to the start.
		if cap(k.buf) > 1024 && cap(k.buf)-len(k.buf) > cap(k.buf)/2 {
			fresh := make([]byte, len(k.buf), cap(k.buf)/2+len(k.buf))
			copy(fresh, k.buf)
			k.buf = fresh
		}
	}
}

// Reset clears the splitter's internal buffer. Call after reconnects so a
// half-frame from a previous session isn't concatenated with new bytes.
func (k *KISSFrameSplitter) Reset() {
	k.buf = k.buf[:0]
}

// bytesIndexByte is the stdlib bytes.IndexByte indirection (kept inlined here
// to avoid a bytes import in this small file).
func bytesIndexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
