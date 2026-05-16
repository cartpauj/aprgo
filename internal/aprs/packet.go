package aprs

import "aprgo/internal/ax25"

// Packet is a fully-decoded APRS packet: the wire frame plus its info-field
// semantics. It is the canonical event flowing on bus.Packets.
type Packet struct {
	Frame   ax25.Frame
	Decoded Decoded
}

// Parse decodes the info field of a Frame, returning a Packet.
// Decode never errors; fields that can't be lifted are left zero.
// Dest is passed because Mic-E encoding hides part of the payload there.
func Parse(f ax25.Frame) Packet {
	return Packet{
		Frame:   f,
		Decoded: Decode(string(f.Info), f.Dest),
	}
}
