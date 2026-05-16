// AX.25 UI frame encode/decode for APRS.
// Only the subset we need: UI frames (control=0x03, pid=0xF0).
package ax25

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxDigipeaters is the AX.25 spec limit on the digipeater list length.
const MaxDigipeaters = 8

// EncodeUIFrame builds an AX.25 UI frame from TNC2-format pieces.
//
//	src    "N0CALL-10"
//	dest   "APRGO"  (or any APRS tocall)
//	digis  ["WIDE1-1", "WIDE2-2"]   (each optionally suffixed "*" for "used")
//	info   the info field bytes
//
// Returns the frame bytes (with NO leading 0x00 KISS port byte and NO FCS).
// Suitable to pass to EncodeKISS.
func EncodeUIFrame(src, dest string, digis []string, info []byte) ([]byte, error) {
	if len(digis) > MaxDigipeaters {
		return nil, fmt.Errorf("too many digipeaters: %d (max %d)", len(digis), MaxDigipeaters)
	}
	out := make([]byte, 0, 14+7*len(digis)+2+len(info))

	d, err := encodeCallsign(dest, false, false)
	if err != nil {
		return nil, fmt.Errorf("dest: %w", err)
	}
	out = append(out, d...)
	last := len(digis) == 0
	s, err := encodeCallsign(src, last, false)
	if err != nil {
		return nil, fmt.Errorf("src: %w", err)
	}
	out = append(out, s...)
	for i, dg := range digis {
		isLast := i == len(digis)-1
		used := strings.HasSuffix(dg, "*")
		dg = strings.TrimSuffix(dg, "*")
		b, err := encodeCallsign(dg, isLast, used)
		if err != nil {
			return nil, fmt.Errorf("digi[%d]: %w", i, err)
		}
		out = append(out, b...)
	}
	out = append(out, 0x03) // UI control
	out = append(out, 0xF0) // PID = No layer 3
	out = append(out, info...)
	return out, nil
}

// encodeCallsign produces the 7-byte shifted-left AX.25 address field.
//
//	last=true sets the low bit of the SSID byte (end of address chain).
//	used=true sets the H bit in the SSID byte (digi has repeated this packet).
func encodeCallsign(callSSID string, last, used bool) ([]byte, error) {
	call, ssid, err := splitCallSSID(callSSID)
	if err != nil {
		return nil, err
	}
	if len(call) > 6 {
		return nil, fmt.Errorf("callsign too long: %q", call)
	}
	b := make([]byte, 7)
	// Pad call to 6 chars with spaces, uppercase, then shift left
	padded := call + strings.Repeat(" ", 6-len(call))
	padded = strings.ToUpper(padded)
	for i := 0; i < 6; i++ {
		b[i] = padded[i] << 1
	}
	ssidByte := byte(0b01100000) | (byte(ssid&0x0F) << 1)
	if last {
		ssidByte |= 0x01
	}
	if used {
		ssidByte |= 0x80
	}
	b[6] = ssidByte
	return b, nil
}

func splitCallSSID(s string) (string, int, error) {
	parts := strings.SplitN(s, "-", 2)
	call := strings.TrimSpace(parts[0])
	if call == "" {
		return "", 0, fmt.Errorf("empty callsign")
	}
	if len(call) > 6 {
		return "", 0, fmt.Errorf("callsign too long: %q", call)
	}
	// AX.25 callsign chars are uppercase A-Z + digits 0-9 only.
	for i := 0; i < len(call); i++ {
		c := call[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		default:
			return "", 0, fmt.Errorf("invalid char %q in callsign %q", c, call)
		}
	}
	ssid := 0
	if len(parts) == 2 {
		v, err := strconv.Atoi(parts[1])
		if err != nil || v < 0 || v > 15 {
			return "", 0, fmt.Errorf("bad ssid: %q", parts[1])
		}
		ssid = v
	}
	return call, ssid, nil
}

// DecodeUIFrame parses a UI frame (no leading KISS type byte, no FCS) into
// TNC2 fields. Non-UI frames return an error.
func DecodeUIFrame(b []byte) (src, dest string, digis []string, info []byte, err error) {
	if len(b) < 16 {
		return "", "", nil, nil, fmt.Errorf("frame too short")
	}
	dest, last := decodeCallsign(b[0:7])
	if last {
		return "", "", nil, nil, fmt.Errorf("no source address")
	}
	src, last = decodeCallsign(b[7:14])
	pos := 14
	for !last && pos+7 <= len(b) && len(digis) < MaxDigipeaters {
		c, l := decodeCallsign(b[pos : pos+7])
		// Restore "*" marker for used digis
		if b[pos+6]&0x80 != 0 {
			c += "*"
		}
		digis = append(digis, c)
		last = l
		pos += 7
	}
	if !last {
		return "", "", nil, nil, fmt.Errorf("address field exceeded %d digis without end-of-address bit", MaxDigipeaters)
	}
	if pos+2 > len(b) {
		return "", "", nil, nil, fmt.Errorf("no control/pid")
	}
	ctrl := b[pos]
	pid := b[pos+1]
	// UI control is 0x03; some TNCs set the P/F bit (0x10) → accept 0x13 too.
	if (ctrl & ^byte(0x10)) != 0x03 || pid != 0xF0 {
		return "", "", nil, nil, fmt.Errorf("not a UI/no-L3 frame (ctrl=0x%02x pid=0x%02x)", ctrl, pid)
	}
	info = b[pos+2:]
	return
}

func decodeCallsign(b []byte) (string, bool) {
	chars := make([]byte, 6)
	for i := 0; i < 6; i++ {
		chars[i] = b[i] >> 1
	}
	call := strings.TrimRight(string(chars), " ")
	ssid := int(b[6]>>1) & 0x0F
	last := b[6]&0x01 != 0
	if ssid != 0 {
		return fmt.Sprintf("%s-%d", call, ssid), last
	}
	return call, last
}

