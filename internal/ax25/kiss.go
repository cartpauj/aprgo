// Package ax25 contains AX.25 UI frame encode/decode and KISS framing.
package ax25

const (
	FEND  = 0xC0
	FESC  = 0xDB
	TFEND = 0xDC
	TFESC = 0xDD
)

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
		if seg[i] == FESC && i+1 < len(seg) {
			switch seg[i+1] {
			case TFEND:
				out = append(out, FEND)
			case TFESC:
				out = append(out, FESC)
			default:
				out = append(out, seg[i+1])
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
