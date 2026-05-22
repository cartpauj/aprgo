// APRS info-field parser.
//
// Position, Mic-E and uncompressed-position decoders ported from aprx's
// parse_aprs.c (MIT licensed, (c) Matti Aarnio OH2MQK 2007-2014), which in
// turn derived from Heikki Hannikainen's Ham::APRS::FAP. We keep aprx's
// rigorous character validation and position-ambiguity handling, then add
// our own UI-facing extras (comment cleanup, message decoding).
package aprs

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Decoded holds what we lift from a packet for display/map use.
type Decoded struct {
	Lat, Lon   *float64
	Symbol     string // 2 chars: table + code (e.g. "/>")
	SymbolName string // friendly name, e.g. "Mobile" or "Tx-iGate"
	Comment    string
	Altitude   int     // feet, 0 if unknown
	Speed      int     // mph, -1 if unknown
	Course     int     // degrees, -1 if unknown
	Ambiguity  int     // 0-4; uncompressed-position only. 0 = full precision.
	// BulletinGroup is set when MsgTo matches the bulletin form `BLNxAAAAA`
	// (APRS101 §14): byte 4 is the identifier char (`0`-`9` numbered,
	// `A`-`Z` announcement), bytes 5-9 optional group name (alphanumeric,
	// space-padded). Empty when MsgTo is not a bulletin. Lets UI render
	// bulletins as broadcasts instead of misclassifying them as 1:1 msgs.
	BulletinGroup string
	Frequency  string  // e.g. "144.390 MHz", empty if unknown
	// FreqTone: CTCSS/PL tone in Hz (e.g. "100.0", "127.3"), DCS code with
	// "D" prefix, or empty. Parsed from `Txxx` / `Dnnn` per Bruninga
	// freqspec.txt — the "front-panel" form used by Kenwood D7/D710 etc.
	FreqTone string
	// FreqOffset: repeater offset, e.g. "+600" / "-600" (kHz), or empty.
	FreqOffset string
	Status     string  // Mic-E status string, e.g. "In Service"
	// MicEFixOld is true when the Mic-E packet declared its GPS fix as
	// "old" (data type indicators 0x1E/0x1F per APRS101 §10.1.1) rather
	// than current (0x1C/0x1D). Lets the UI render a "GPS data is stale"
	// hint instead of silently rendering a possibly-outdated position.
	MicEFixOld bool
	IsMessage  bool
	MsgTo      string
	MsgBody    string
	MsgID      string
	IsAck      bool
	IsRej      bool
	AckedID    string

	// ReplyAckID is the piggyback ack from APRS 1.1 reply-ack form
	// (`body{MM}AA`). When non-empty, the sender is acknowledging their
	// peer's outgoing msgID AA at the same time as sending their own MM.
	ReplyAckID string

	// MsgOrigSrc is set when the packet was wrapped in a third-party header
	// (info starts with "}SRC>DEST,PATH:..."). Holds the inner SRC so the UI
	// can show the actual originator instead of the AX.25-layer relay station.
	// Empty for non-third-party packets.
	MsgOrigSrc string

	// Telemetry (T#NNN,a1,a2,a3,a4,a5,bbbbbbbb)
	IsTelemetry  bool
	TelemSeq     int
	TelemAnalog  [5]float64
	TelemBits    [8]bool

	// TelemConfig is non-nil when the packet is a telemetry-configuration
	// message (PARM. / UNIT. / EQNS. / BITS.) per APRS101 §13.4. These
	// look like 1:1 messages on the wire but describe the labels / units /
	// coefficients / sense bits for a station's T# data — applying them
	// turns generic "Analog 0..4" into properly named channels.
	TelemConfig *TelemConfig

	// Weather is non-nil when a positional weather report was decoded out
	// of the comment. Fields are populated per APRS spec §12; check the
	// *Set flags on individual subfields to distinguish "not reported"
	// from "reported as 0."
	Weather *Weather

	// PHG is non-nil when a PHGxxxx code was decoded. Carries
	// power/height/gain/directivity + a derived range estimate in miles.
	PHG *PHG

	// RNG is non-nil when an explicit RNGxxxx range circle was decoded.
	RNG *RNG

	// ObjectKilled is true when an object report (`;`) or item report (`)`)
	// was marked as killed (live/kill flag is `_` rather than `*`/`!`).
	ObjectKilled bool

	// ObjectName is the 9-char object name (trimmed) for `;` packets, or
	// the variable-length (3-9 char) item name for `)` packets. Empty for
	// non-object/item packets. Per APRS101 §11, object names are the most
	// useful field on these packets (typically a repeater identifier, WX
	// station name, etc.) — surfacing them is what lets the UI show e.g.
	// "446.25HRM" instead of just "killed object".
	ObjectName string

	// isMicE is set when decodeMicE produced this struct. Used to scope
	// Mic-E-only post-processing (the "_X" status code trim) so non-Mic-E
	// packets ending in "_X" don't pick up spurious Status fields.
	isMicE bool
}

// Decode parses one info field. dest is the AX.25 destination address (used
// for Mic-E lat decoding).
func Decode(info, dest string) Decoded {
	return decodeWithDepth(info, dest, 0)
}

const maxThirdPartyDepth = 4

func decodeWithDepth(info, dest string, depth int) Decoded {
	d := Decoded{Speed: -1, Course: -1}
	if info == "" {
		return d
	}

	// 3rd-party packets ('}'): skip the outer header and parse the inner.
	//   }SRC>DEST,path:ACTUAL_INFO
	if info[0] == '}' {
		if depth >= maxThirdPartyDepth {
			return d
		}
		if colon := strings.Index(info, ":"); colon >= 0 && colon+1 < len(info) {
			inner := info[colon+1:]
			innerDest := dest
			innerSrc := ""
			if gt := strings.Index(info[1:colon], ">"); gt >= 0 {
				innerSrc = info[1 : 1+gt]
				path := info[1+gt+1 : colon]
				if comma := strings.Index(path, ","); comma >= 0 {
					innerDest = path[:comma]
				} else {
					innerDest = path
				}
			}
			sub := decodeWithDepth(inner, innerDest, depth+1)
			// Preserve the original source from the third-party header so the
			// UI can show e.g. "From: SMS (relayed by KB7COX-10)" rather than
			// just the AX.25 relay station's call.
			if sub.MsgOrigSrc == "" {
				sub.MsgOrigSrc = innerSrc
			}
			return sub
		}
	}

	switch info[0] {
	case '!', '=':
		decodeUncompressedOrCompressed(info[1:], &d)
	case '/', '@':
		// position with timestamp; skip 7-char timestamp `DDHHMM[zh/]`.
		// APRS101 §5 also lists pre-1.0 DTIs `/1`..`/9` (raw GPS-port
		// passthrough); spec footnote marks them "reserved, do not transmit"
		// and no modern client emits them. A real timestamp has the form
		// 6 digits + suffix in {z,h,/}; legacy /N doesn't match.
		if len(info) >= 8 && looksLikeTimestamp(info[1:8]) {
			decodeUncompressedOrCompressed(info[8:], &d)
		}
	case ';':
		// Object: ; + 9-char name + live/kill (* or _) + 7-char timestamp + position
		if len(info) > 18 {
			d.ObjectName = strings.TrimRight(info[1:10], " ")
			if info[10] == '_' {
				d.ObjectKilled = true
			}
			decodeUncompressedOrCompressed(info[18:], &d)
		}
	case ')':
		// item: )NAME!_pos... or )NAME_pos... — APRS101 §11 says NAME is
		// 3-9 chars and may not contain `!`, `_`, or `*`. Enforce length
		// here so a malformed `)X!...` (1-char name) doesn't decode bogus
		// position from chars that should have been the name body.
		if i := strings.IndexAny(info[1:], "!_"); i >= 3 && i <= 9 && len(info) > i+2 {
			d.ObjectName = info[1 : 1+i]
			if info[1+i] == '_' {
				d.ObjectKilled = true
			}
			decodeUncompressedOrCompressed(info[i+2:], &d)
		}
	case '_':
		// Positionless weather report per APRS101 §12.5:
		//   _MMDDHHMM<weather fields>
		// Position is implicit (advertised separately, usually as an Object).
		// Strip the 8-byte MMDDHHMM timestamp and run the weather parser on
		// the rest. No symbol is implied; leave Symbol unset.
		if len(info) > 9 {
			if w, _ := parseWeather(info[9:]); w != nil {
				d.Weather = w
			}
		}
	case '$':
		// Raw NMEA-0183 sentence per APRS101 §5. Common forms: $GPRMC
		// (recommended minimum: lat, lon, speed-knots, course) and $GPGGA
		// (lat, lon, MSL altitude meters). Legacy/cheap trackers that
		// don't bother encoding APRS position strings emit raw NMEA here.
		decodeNMEA(info, &d)
	case '`', '\'':
		decodeMicE(info, dest, &d)
	case ':':
		decodeMessage(info[1:], &d)
	case '>':
		// APRS101 §16: status report is `>Comments` or `>DDHHMMzComments`
		// where the optional 7-byte timestamp is 6 digits + literal 'z'.
		// Strip when present so display shows the status text, not the
		// timestamp prefix.
		rest := info[1:]
		if len(rest) >= 7 && rest[6] == 'z' && isAllDigits(rest[:6]) {
			rest = rest[7:]
		}
		d.Comment = strings.TrimRight(rest, "\r\n")
	case 'T':
		if strings.HasPrefix(info, "T#") {
			decodeTelemetry(info[2:], &d)
		}
	}
	postProcess(&d)
	return d
}

// postProcess extracts secondary data from the comment (altitude, frequency,
// Mic-E status code, course/speed in NNN/NNN form) and sets SymbolName.
func postProcess(d *Decoded) {
	if d.Symbol != "" {
		d.SymbolName = symbolNameOf(d.Symbol)
	}
	// Sanitize free-form text fields regardless of whether Comment is set.
	// Object names / message bodies may carry 8-bit chars even when Comment
	// is empty.
	d.MsgBody = sanitizeText(d.MsgBody)
	d.ObjectName = sanitizeText(d.ObjectName)
	if d.Comment == "" {
		return
	}
	c := d.Comment

	// Altitude: /A=NNNNNN (feet) anywhere in comment, usually at the end.
	if m := altRegex.FindStringSubmatchIndex(c); m != nil {
		if v, err := strconv.Atoi(c[m[2]:m[3]]); err == nil {
			d.Altitude = v
		}
		c = strings.TrimSpace(c[:m[0]] + c[m[1]:])
	}

	// Course/Speed at the START of the comment: "NNN/NNN" course/knots.
	// Skip entirely for weather-symbol stations (`_` as the symbol code) —
	// for them the leading NNN/NNN is wind direction/speed, not motion.
	// Weather parser below picks it up correctly.
	isWXSym := len(d.Symbol) >= 2 && d.Symbol[1] == '_'
	if d.Speed < 0 && !isWXSym {
		if m := csRegex.FindStringSubmatch(c); m != nil {
			course, _ := strconv.Atoi(m[1])
			knots, _ := strconv.Atoi(m[2])
			// APRS convention: valid course range is 1-360 (360=north),
			// 0 is reserved as "course unknown" (Ham::APRS::FAP). Leave
			// d.Course = -1 (unknown) when raw value is 0.
			if course >= 1 && course <= 360 {
				d.Course = course
			}
			d.Speed = int(float64(knots) * 1.15078)
			c = strings.TrimSpace(c[len(m[0]):])
		}
	}

	// Frequency: NNN.NNNMHz with optional space
	if m := freqRegex.FindStringSubmatchIndex(c); m != nil {
		freq := strings.TrimSpace(c[m[2]:m[3]])
		d.Frequency = freq + " MHz"
		// don't strip from comment — operators usually want to keep it visible
		// Per Bruninga freqspec.txt, the front-panel form follows the
		// frequency with optional CTCSS tone (T100, T127), DCS code
		// (Dnnn), and offset (+600/-600 in kHz). Extract when present.
		tail := c[m[3]:]
		if tm := freqToneRegex.FindStringSubmatch(tail); tm != nil {
			if tm[1] != "" {
				d.FreqTone = tm[1] // CTCSS Hz (digits)
			} else if tm[2] != "" {
				d.FreqTone = "D" + tm[2] // DCS code
			}
		}
		if om := freqOffsetRegex.FindStringSubmatch(tail); om != nil {
			// Per Bruninga freqspec.txt the digits are 10s-of-kHz, so
			// "+060" → 600 kHz → +0.600 MHz, "-005" → 50 kHz → -0.050 MHz.
			// Format as MHz with three decimals so the unit is unambiguous.
			if khz, err := strconv.Atoi(om[2]); err == nil {
				mhz := float64(khz) * 10 / 1000.0
				d.FreqOffset = om[1] + strconv.FormatFloat(mhz, 'f', 3, 64) + " MHz"
			}
		}
	}

	// Mic-E status code at end: "_X" where X is one of 0-9, :, ;, <, =.
	// Only applies to Mic-E packets to avoid spurious matches on regular
	// comments that happen to end in "_0" etc.
	if d.isMicE && len(c) >= 2 && c[len(c)-2] == '_' {
		code := c[len(c)-1]
		if name, ok := miceStatusCodes[code]; ok {
			d.Status = name
			c = strings.TrimRight(c[:len(c)-2], " \t")
		}
	}

	// Weather, PHG, RNG: each pattern strips its matched span from the
	// comment so the parsed fields and the residual comment don't
	// double-display. WX stations are gated on the `_` symbol code to
	// avoid mis-parsing positional comments that happen to match the
	// weather regex shape.
	if isWXSym || looksLikeWeather(c) {
		if w, stripped := parseWeather(c); w != nil {
			d.Weather = w
			c = stripped
		}
	}
	if p, stripped := parsePHG(c); p != nil {
		d.PHG = p
		c = stripped
	}
	if r, stripped := parseRNG(c); r != nil {
		d.RNG = r
		c = stripped
	}

	// !DAO! micro-precision extension per aprs.org/aprs12/datum.txt:
	//   !<Datum><LatPrec><LonPrec>!
	// Datum char (W=WGS84, etc) plus two precision bytes. Two encodings:
	//   Uppercase: ASCII digit '0'-'9' → +d/10 of the last decoded
	//              decimal-minute digit (1/1000 minute, ~18cm at equator).
	//   Lowercase: base-91 char '!'-'{' (33-123) → +(c-33)/91 of the last
	//              decimal-minute digit (~2cm precision).
	// Found embedded anywhere in the comment by ~20-40% of modern radios
	// (Kenwood TH-D74/75, Yaesu FTM-400/500, APRSdroid). Refine the existing
	// lat/lon by the indicated extra precision and strip the marker.
	if d.Lat != nil && d.Lon != nil {
		if m := daoRegex.FindStringSubmatchIndex(c); m != nil {
			match := c[m[0]:m[1]]
			// match: "!D<lat><lon>!"
			datum := match[1]
			latByte := match[2]
			lonByte := match[3]
			latFrac, latOK := daoFraction(latByte)
			lonFrac, lonOK := daoFraction(lonByte)
			if latOK && lonOK {
				// Extra precision applies to 1/100 of a minute (the last
				// digit of the decoded position's decimal-minute pair).
				// At 1/100 min = 0.01/60 deg per minute, the extra digit
				// adds 0.01*frac/60 of a degree.
				const minToDeg = 1.0 / 60.0
				extraLat := 0.01 * latFrac * minToDeg
				extraLon := 0.01 * lonFrac * minToDeg
				if *d.Lat < 0 {
					extraLat = -extraLat
				}
				if *d.Lon < 0 {
					extraLon = -extraLon
				}
				newLat := *d.Lat + extraLat
				newLon := *d.Lon + extraLon
				d.Lat = &newLat
				d.Lon = &newLon
			}
			_ = datum // we don't surface non-WGS84 datums; just consume the byte
			c = strings.TrimSpace(c[:m[0]] + c[m[1]:])
		}
	}

	// Compact base-91 telemetry per he.fi/doc/aprs-base91-comment-telemetry.txt:
	//   |ssaaaaaaaa|        (seq + 1-5 analog channels, each 2 base-91 chars)
	// Each 2-char pair encodes 0..8280 = (c0-33)*91 + (c1-33). Skip if the
	// run is the wrong length or contains non-base-91 chars — falls through
	// silently so a literal `|...|` in a free-form comment isn't misread.
	if i := strings.IndexByte(c, '|'); i >= 0 {
		if j := strings.IndexByte(c[i+1:], '|'); j > 0 {
			block := c[i+1 : i+1+j]
			if n := len(block); n >= 4 && n <= 12 && n%2 == 0 {
				valid := true
				for k := 0; k < n; k++ {
					if block[k] < 0x21 || block[k] > 0x7b {
						valid = false
						break
					}
				}
				if valid {
					d.IsTelemetry = true
					seq := (int(block[0])-33)*91 + int(block[1]-33)
					d.TelemSeq = seq
					channels := (n - 2) / 2
					for a := 0; a < channels && a < 5; a++ {
						p := 2 + a*2
						d.TelemAnalog[a] = float64((int(block[p])-33)*91 + int(block[p+1]-33))
					}
					c = c[:i] + c[i+1+j+1:]
				}
			}
		}
	}

	// Final pass: sanitize Comment. Strips control chars and converts
	// Latin-1 8-bit characters (°, ·, ±, etc.) to UTF-8 so they render
	// correctly in the web UI. Applied here at the end so the per-DTI
	// parsers and regex post-processing above can work on raw bytes
	// before encoding conversion.
	d.Comment = sanitizeText(c)
}

// looksLikeWeather is a cheap pre-filter: returns true when the comment
// contains at least three weather-field markers in a row. Lets WX-encoded
// comments on non-`_`-symbol stations (some operators put weather data on
// their default symbol) still be parsed without triggering false positives
// on arbitrary text.
func looksLikeWeather(c string) bool {
	hits := 0
	for _, k := range []byte{'g', 't', 'r', 'p', 'P', 'h', 'b'} {
		if i := strings.IndexByte(c, k); i >= 0 {
			// Quick check: followed by 2+ digits.
			if i+3 <= len(c) && c[i+1] >= '0' && c[i+1] <= '9' && c[i+2] >= '0' && c[i+2] <= '9' {
				hits++
				if hits >= 3 {
					return true
				}
			}
		}
	}
	return false
}

var (
	altRegex  = regexp.MustCompile(`/A=([0-9]{6})`)
	csRegex   = regexp.MustCompile(`^([0-9]{3})/([0-9]{3})`)
	freqRegex = regexp.MustCompile(`\b(\d{2,3}\.\d{3,4})\s?MHz\b`)
	// Tone: `T100` (CTCSS Hz, omit decimal point) OR `D023` (DCS).
	freqToneRegex = regexp.MustCompile(`\bT(\d{2,3})\b|\bD(\d{3})\b`)
	// Offset: `+600` or `-600` (kHz). Bruninga freqspec.txt.
	freqOffsetRegex = regexp.MustCompile(`(?:^|\s)([+-])(\d{3,4})(?:\s|$)`)
	// !DAO! datum/precision extension per aprs12/datum.txt: 5 chars total,
	// `!` + datum letter + lat-precision byte + lon-precision byte + `!`.
	// Datum: uppercase (W, G, etc) or lowercase variants.
	// Precision bytes: uppercase '0'-'9' (ASCII digit) or lowercase '!'-'{'
	// (base-91 33-123). We accept the union of both byte classes here and
	// validate the encoding inside daoFraction.
	daoRegex = regexp.MustCompile(`!([A-Za-z])([!-{])([!-{])!`)
)

// daoFraction interprets one !DAO! precision byte. Returns (fraction in
// [0,1), ok). Uppercase ASCII digits give d/10; lowercase base-91 chars
// (range 33-123) give (c-33)/91.
func daoFraction(b byte) (float64, bool) {
	if b >= '0' && b <= '9' {
		return float64(b-'0') / 10.0, true
	}
	if b >= 0x21 && b <= 0x7b {
		return float64(b-33) / 91.0, true
	}
	return 0, false
}

// Mic-E status codes — single-char shorthand at end of comment.
var miceStatusCodes = map[byte]string{
	'0': "Off Duty",
	'1': "En Route",
	'2': "In Service",
	'3': "Returning",
	'4': "Committed",
	'5': "Special",
	'6': "Priority",
	'7': "Custom-1",
	'8': "Custom-2",
	'9': "Custom-3",
	':': "Custom-4",
	';': "Custom-5",
	'<': "Custom-6",
	'=': "Emergency",
}

// symbolNameOf returns a human-readable name for the 2-char APRS symbol.
// Covers the common subset; falls back to the raw code for unknown symbols.
func symbolNameOf(sym string) string {
	if name, ok := primarySymbols[sym]; ok {
		return name
	}
	if name, ok := alternateSymbols[sym]; ok {
		return name
	}
	// Overlay form: any non-'/' non-'\' table char with a code from alternate table
	if len(sym) == 2 && sym[0] != '/' && sym[0] != '\\' {
		if name, ok := alternateSymbols[`\`+string(sym[1])]; ok {
			return name + " (" + string(sym[0]) + ")"
		}
	}
	return ""
}

// Primary table '/' — common symbols
var primarySymbols = map[string]string{
	"/!": "Police Station", "/#": "Digi", "/$": "Phone", "/%": "DX Cluster",
	"/&": "Gateway", "/'": "Small Aircraft", "/(": "Mobile Satellite",
	"/*": "Snowmobile", "/+": "Red Cross", "/,": "Boy Scouts", "/-": "House (QTH)",
	"/.": "X", "//": "Dot", "/0": "Numbered 0", "/1": "Numbered 1",
	"/<": "Motorcycle", "/=": "Railroad Engine", "/>": "Car",
	"/?": "File Server", "/@": "Hurricane", "/A": "Aid Station", "/B": "BBS",
	"/C": "Canoe", "/E": "Eyeball", "/F": "Farm Vehicle", "/G": "Grid Square",
	"/H": "Hotel", "/I": "TCP/IP", "/J": "Jet", "/K": "School",
	"/L": "PC User", "/M": "MacAPRS", "/N": "NTS Station", "/O": "Balloon",
	"/P": "Police", "/Q": "Quake", "/R": "Recreational Vehicle", "/S": "Shuttle",
	"/T": "SSTV", "/U": "Bus", "/V": "ATV", "/W": "National Weather Service",
	"/X": "Helicopter", "/Y": "Yacht", "/Z": "WinAPRS", "/[": "Jogger",
	"/\\": "Triangle", "/]": "PBBS", "/^": "Aircraft (Large)",
	"/_": "Weather Station", "/`": "Dish Antenna", "/a": "Ambulance",
	"/b": "Bike", "/c": "Incident Command Post", "/d": "Fire Station",
	"/e": "Horse", "/f": "Fire Truck", "/g": "Glider", "/h": "Hospital",
	"/i": "IOTA", "/j": "Jeep", "/k": "Truck", "/l": "Laptop",
	"/m": "Mic-Repeater", "/n": "Node", "/o": "EOC", "/p": "Rover (Dog)",
	"/q": "Grid Square shown above 128m", "/r": "Repeater", "/s": "Ship",
	"/t": "Truck Stop", "/u": "Truck (18-wheeler)", "/v": "Van",
	"/w": "Water Station", "/x": "X-APRS (Unix)", "/y": "Yagi", "/z": "Shelter",
}

// Alternate table '\' — common symbols
var alternateSymbols = map[string]string{
	"\\!": "Emergency", "\\#": "Digi (overlay)", "\\$": "Bank", "\\%": "Power Plant",
	"\\&": "Tx-iGate", "\\'": "Aircraft", "\\(": "Cloudy",
	"\\*": "Avalanche", "\\+": "Church", "\\,": "Girl Scouts", "\\-": "House (HF)",
	"\\<": "Gale Flags", "\\=": "Truck (Aprstt)", "\\>": "Car (overlay)",
	"\\?": "Info", "\\@": "Hurricane Forecast", "\\A": "Box", "\\B": "Blowing Snow",
	"\\C": "Coast Guard", "\\D": "Drizzle", "\\E": "Smoke", "\\F": "Freezing Rain",
	"\\G": "Snow Shower", "\\H": "Haze", "\\I": "Rain Shower", "\\J": "Lightning",
	"\\K": "Kenwood", "\\L": "Lighthouse", "\\M": "MARS", "\\N": "Navigation Buoy",
	"\\O": "Rocket", "\\P": "Parking", "\\Q": "Earthquake", "\\R": "Restaurant",
	"\\S": "Sat/Pacsat", "\\T": "Thunderstorm", "\\U": "Sunny",
	"\\V": "VORTAC", "\\W": "NWS Site", "\\X": "Pharmacy", "\\Y": "Radios",
	"\\[": "Wall Cloud", "\\^": "Aircraft (Mil)", "\\_": "Weather Station (NWS)",
	"\\`": "Rain", "\\a": "ARRL", "\\b": "Blowing Dust", "\\c": "CD Triangle",
	"\\d": "DX Spot", "\\e": "Sleet", "\\f": "Funnel Cloud", "\\g": "Gale",
	"\\h": "Store", "\\i": "Black Box", "\\j": "Workzones", "\\k": "Truck",
	"\\l": "Area Locations", "\\m": "Value Sign", "\\n": "Triangle (Overlay)",
	"\\o": "Small Circle", "\\p": "Partly Cloudy", "\\q": "Quake",
	"\\r": "Restrooms", "\\s": "Ship/Boat", "\\t": "Tornado",
	"\\u": "Truck (Overlay)", "\\v": "Van (Overlay)", "\\w": "Flooding",
	"\\x": "Wreck", "\\y": "Skywarn", "\\z": "Shelter",
}

// decodeUncompressedOrCompressed picks the right parser for what follows a
// position-type indicator. APRS uses the first character to discriminate:
// digit/space → uncompressed; printable letter/symbol → compressed.
func decodeUncompressedOrCompressed(s string, d *Decoded) {
	if len(s) < 1 {
		return
	}
	first := s[0]
	if (first >= '0' && first <= '9') || first == ' ' {
		decodeUncompressed(s, d)
	} else {
		decodeCompressed(s, d)
	}
}

// ---- Uncompressed position (ported from aprx parse_aprs_uncompressed) ----
//
// Format: DDMM.mmH/DDDMM.mmHC where:
//   DDMM.mm = lat (degrees, mins, hundredths-of-min, spaces allowed for ambiguity)
//   H       = N or S
//   /       = symbol table
//   DDDMM.mm = lon
//   H       = E or W
//   C       = symbol code

func decodeUncompressed(s string, d *Decoded) {
	if len(s) < 19 {
		return
	}
	pos := []byte(s[:19])
	// Position ambiguity: spaces become specific digits per APRS spec §6.
	// Count lat-side blanks to derive level (0..4) so consumers can render
	// "approximate location" without recomputing.
	//   level 1: pos[6]   blank (~185 m)
	//   level 2: pos[5,6] blank (~1.8 km)
	//   level 3: pos[3,5,6] blank (~18 km)
	//   level 4: pos[2,3,5,6] blank (~111 km)
	amb := 0
	if pos[6] == ' ' {
		amb = 1
	}
	if pos[5] == ' ' {
		amb = 2
	}
	if pos[3] == ' ' {
		amb = 3
	}
	if pos[2] == ' ' {
		amb = 4
	}
	d.Ambiguity = amb
	if pos[2] == ' ' {
		pos[2] = '3'
	}
	if pos[3] == ' ' {
		pos[3] = '5'
	}
	if pos[5] == ' ' {
		pos[5] = '5'
	}
	if pos[6] == ' ' {
		pos[6] = '5'
	}
	if pos[12] == ' ' {
		pos[12] = '3'
	}
	if pos[13] == ' ' {
		pos[13] = '5'
	}
	if pos[15] == ' ' {
		pos[15] = '5'
	}
	if pos[16] == ' ' {
		pos[16] = '5'
	}

	var latDeg, latMin, latMinFrag int
	var latH, symTable byte
	var lonDeg, lonMin, lonMinFrag int
	var lonH, symCode byte
	n, _ := fmt.Sscanf(string(pos), "%2d%2d.%2d%c%c%3d%2d.%2d%c%c",
		&latDeg, &latMin, &latMinFrag, &latH, &symTable,
		&lonDeg, &lonMin, &lonMinFrag, &lonH, &symCode)
	if n != 10 {
		return
	}
	if !validSymTableUncompressed(symTable) {
		return
	}
	if latH != 'N' && latH != 'S' && latH != 'n' && latH != 's' {
		return
	}
	if lonH != 'E' && lonH != 'W' && lonH != 'e' && lonH != 'w' {
		return
	}
	if latDeg > 90 || lonDeg > 180 || latMin >= 60 || lonMin >= 60 ||
		latMinFrag > 99 || lonMinFrag > 99 {
		return
	}
	lat := float64(latDeg) + float64(latMin)/60.0 + float64(latMinFrag)/6000.0
	lon := float64(lonDeg) + float64(lonMin)/60.0 + float64(lonMinFrag)/6000.0
	if latH == 'S' || latH == 's' {
		lat = -lat
	}
	if lonH == 'W' || lonH == 'w' {
		lon = -lon
	}
	d.Lat = &lat
	d.Lon = &lon
	d.Symbol = string(symTable) + string(symCode)
	if len(s) > 19 {
		d.Comment = strings.TrimRight(s[19:], "\r\n")
	}
}

// ---- Compressed position (ported from aprx parse_aprs_compressed) ----
//
// 13 bytes: SYMtable LAT(4) LON(4) SYMcode CSPEED(2) CMPRTYPE

func decodeCompressed(s string, d *Decoded) {
	if len(s) < 13 {
		return
	}
	symTable := s[0]
	if !validSymTableCompressed(symTable) {
		return
	}
	for i := 1; i <= 8; i++ {
		if s[i] < 0x21 || s[i] > 0x7b {
			return
		}
	}
	lat1 := int(s[1]-33)*91*91*91 + int(s[2]-33)*91*91 + int(s[3]-33)*91 + int(s[4]-33)
	lon1 := int(s[5]-33)*91*91*91 + int(s[6]-33)*91*91 + int(s[7]-33)*91 + int(s[8]-33)
	lat := 90.0 - float64(lat1)/380926.0
	lon := -180.0 + float64(lon1)/190463.0
	symCode := s[9]
	d.Lat = &lat
	d.Lon = &lon
	d.Symbol = string(symTable) + string(symCode)

	// cs (bytes 10-11) and T (byte 12) per APRS101 Ch. 9.
	// cs[0] == ' ' (0x20) signals "no extension data" — csT are ignored.
	// cs[0] == '{' (0x7B) means cs encodes a range circle in miles.
	// T-byte NMEA-source bits 3-4 == 10 (GGA) means cs encodes altitude.
	// Otherwise (cs[0] in '!'-'z' i.e. 33-122) cs encodes course/speed.
	// T byte is itself printable: actual_value = ascii - 33.
	c0 := s[10]
	c1 := s[11]
	tByte := int(s[12]) - 33
	switch {
	case c0 == ' ':
		// no extension data
	case c0 == '{':
		// range circle: range_miles = 2 * 1.08 ^ (s-33)
		if c1 >= 0x21 && c1 <= 0x7b {
			r := 2.0 * math.Pow(1.08, float64(c1-33))
			d.RNG = &RNG{Miles: int(r + 0.5)}
		}
	case tByte >= 0 && ((tByte>>3)&0x03) == 0x02:
		// NMEA source = GGA → cs encodes altitude in feet:
		// alt = 1.002 ^ ((c-33)*91 + (s-33))
		if c0 >= 0x21 && c0 <= 0x7b && c1 >= 0x21 && c1 <= 0x7b {
			alt := math.Pow(1.002, float64(int(c0-33)*91+int(c1-33)))
			d.Altitude = int(alt + 0.5)
		}
	default:
		// course/speed: course = (c-33)*4 deg, speed = 1.08^(s-33) - 1 knots
		if c0 >= '!' && c0 <= 'z' && c1 >= 0x21 && c1 <= 0x7b {
			d.Course = int(c0-33) * 4
			knots := math.Pow(1.08, float64(c1-33)) - 1
			d.Speed = int(knots*1.15078 + 0.5) // knots → mph (matches uncompressed)
		}
	}

	if len(s) > 13 {
		d.Comment = strings.TrimRight(s[13:], "\r\n")
	}
}

// ---- Mic-E (ported from aprx parse_aprs_mice with strict validation) ----

func decodeMicE(info, dest string, d *Decoded) {
	if len(info) < 9 || len(dest) < 6 {
		return
	}
	// dest must be ASCII for the byte-indexed decode below; a multibyte char
	// in dest plus a 6-byte slice would crash. Reject quickly.
	for i := 0; i < 6; i++ {
		if dest[i] > 0x7f {
			return
		}
	}
	body := []byte(info[1:]) // skip the ' or ` indicator? actually keep it for now
	// The body indexed 0..7 corresponds to info[1..8] in the original spec,
	// because parse_aprs_mice receives body = info_start+1.
	// Here we operate on info[1:], so body[0] = info[1].
	if len(body) < 8 {
		return
	}
	// Strict info-field byte range checks (verbatim from aprx)
	if body[0] < 0x26 || body[0] > 0x7f {
		return
	}
	if body[1] < 0x26 || body[1] > 0x61 {
		return
	}
	if body[2] < 0x1c || body[2] > 0x7f {
		return
	}
	if body[3] < 0x1c || body[3] > 0x7f {
		return
	}
	if body[4] < 0x1c || body[4] > 0x7d {
		return
	}
	if body[5] < 0x1c || body[5] > 0x7f {
		return
	}
	if (body[6] < 0x21 || body[6] > 0x7b) && body[6] != 0x7d {
		return
	}
	if !validSymTableUncompressed(body[7]) {
		return
	}

	// Destination call validation: first 3 chars in [0-9 A-L P-Z], last 3 in [0-9 L P-Z]
	dst := []byte(strings.ToUpper(dest[:6]))
	for i := 0; i < 3; i++ {
		c := dst[i]
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'L') || (c >= 'P' && c <= 'Z')) {
			return
		}
	}
	for i := 3; i < 6; i++ {
		c := dst[i]
		if !((c >= '0' && c <= '9') || c == 'L' || (c >= 'P' && c <= 'Z')) {
			return
		}
	}

	// Translate dstcall: A-J -> 0-9, P-Y -> 0-9, K/L/Z -> '_' (ambiguity placeholder)
	dstcall := make([]byte, 6)
	for i := 0; i < 6; i++ {
		c := dst[i]
		switch {
		case c >= 'A' && c <= 'J':
			dstcall[i] = c - 'A' + '0'
		case c >= 'P' && c <= 'Y':
			dstcall[i] = c - 'P' + '0'
		case c == 'K', c == 'L', c == 'Z':
			dstcall[i] = '_'
		default:
			dstcall[i] = c
		}
	}

	posambig := 0
	if dstcall[5] == '_' {
		dstcall[5] = '5'
		posambig = 1
	}
	if dstcall[4] == '_' {
		dstcall[4] = '5'
		posambig = 2
	}
	if dstcall[3] == '_' {
		dstcall[3] = '5'
		posambig = 3
	}
	if dstcall[2] == '_' {
		dstcall[2] = '3'
		posambig = 4
	}
	if dstcall[1] == '_' || dstcall[0] == '_' {
		return
	}
	// Surface the ambiguity level on Decoded so UI/consumers can render
	// "approximate location" the same way as for uncompressed positions.
	// Previously posambig was only used internally to pick the lat formula.
	d.Ambiguity = posambig

	latDeg, _ := strconv.Atoi(string(dstcall[0:2]))
	latMin, _ := strconv.Atoi(string(dstcall[2:4]))
	latMinFrag, _ := strconv.Atoi(string(dstcall[4:6]))
	lat := float64(latDeg) + float64(latMin)/60.0 + float64(latMinFrag)/6000.0
	if dst[3] <= 0x4c { // 0..9 or K/L → south
		lat = -lat
	}

	lonDeg := int(body[0]) - 28
	if dst[4] >= 0x50 { // P-Y → +100 offset
		lonDeg += 100
	}
	switch {
	case lonDeg >= 180 && lonDeg <= 189:
		lonDeg -= 80
	case lonDeg >= 190 && lonDeg <= 199:
		lonDeg -= 190
	}
	lonMin := int(body[1]) - 28
	// Per APRS101 §10 / mic-e-types.txt: valid lonMin is 0-59. Encoder may
	// shift by 60 when the raw byte is in the 88-97 range, but values 60-69
	// (post-subtract) are reserved/invalid — silently wrapping them produces
	// bogus positions. Treat as malformed: skip setting lat/lon but continue
	// to harvest status / symbol / comment from the rest of the packet.
	posValid := true
	if lonMin >= 60 && lonMin <= 69 {
		posValid = false
	}
	if lonMin >= 60 {
		lonMin -= 60
	}
	lonMinFrag := int(body[2]) - 28

	var lon float64
	switch posambig {
	case 0:
		lon = float64(lonDeg) + float64(lonMin)/60.0 + float64(lonMinFrag)/6000.0
	case 1:
		lon = float64(lonDeg) + float64(lonMin)/60.0 + float64(lonMinFrag-lonMinFrag%10+5)/6000.0
	case 2:
		lon = float64(lonDeg) + (float64(lonMin)+0.5)/60.0
	case 3:
		lon = float64(lonDeg) + float64(lonMin-lonMin%10+5)/60.0
	case 4:
		lon = float64(lonDeg) + 0.5
	default:
		return
	}
	if dst[5] >= 0x50 {
		lon = -lon
	}
	if posValid {
		d.Lat = &lat
		d.Lon = &lon
	}
	d.Symbol = string(body[7]) + string(body[6])

	// Mic-E speed/course (bytes 3-5 of info, but we operate on body so body[3..5])
	// body[3] = SP+28 (speed in 10s of knots, 0..199kt)
	// body[4] = DC+28 (DC: digit + 10s of course)
	// body[5] = SE+28 (course units)
	if len(body) >= 6 {
		sp := int(body[3]) - 28
		dc := int(body[4]) - 28
		se := int(body[5]) - 28
		spTens := sp * 10
		spUnits := dc / 10
		speedKt := spTens + spUnits
		if speedKt >= 800 {
			speedKt -= 800
		}
		courseHundreds := dc % 10
		course := courseHundreds*100 + se
		if course >= 400 {
			course -= 400
		}
		if speedKt >= 0 && speedKt < 800 {
			d.Speed = int(float64(speedKt) * 1.15078) // knots → mph
		}
		// APRS convention: valid course range is 1-360 (360=north),
		// 0 is reserved as "course unknown" (Ham::APRS::FAP).
		if course >= 1 && course <= 360 {
			d.Course = course
		} else {
			d.Course = -1
		}
	}

	// Mic-E "extended status" follows the symbol bytes. Format examples:
	//   `"Cv}<comment>      (typical: backtick + 2-3 bytes + '}')
	//   ''Bw}<comment>
	//   <0x1d>><comment>    (status byte + comment, no '}')
	// Per APRS101 §10.1.5 altitude is encoded as 3 base-91 chars immediately
	// before a literal '}': altitude_m = (c0-33)*91² + (c1-33)*91 + (c2-33) − 10000.
	// Without this decode the altitude is silently dropped on every Mic-E
	// packet from Kenwood TM-D710 / TH-D74 / Yaesu FT2D-3D etc.
	if len(body) > 8 {
		c := body[8:]
		// Mic-E leading type-code byte (aprs.org/aprs12/mic-e-types.txt):
		// `>` = TH-D7A, `]` = TM-D700. The byte was reserved as protocol
		// metadata at the start of the free-text field for the two original
		// Kenwood radios — strip it so it doesn't show as a stray char at
		// the front of the comment.
		if len(c) > 0 && (c[0] == '>' || c[0] == ']') {
			c = c[1:]
		}
		// Only treat `}` within the first 8 bytes as the altitude terminator
		// when the 3 preceding bytes are valid base-91 — otherwise it's a
		// literal `}` in the comment text and the leading bytes are real
		// comment content (e.g. "73 de KX}").
		altDecoded := false
		if i := indexByteBounded(c, '}', 8); i >= 3 {
			a, b, cc := c[i-3], c[i-2], c[i-1]
			if a >= 0x21 && a <= 0x7b && b >= 0x21 && b <= 0x7b && cc >= 0x21 && cc <= 0x7b {
				altM := (int(a-33)*91+int(b-33))*91 + int(cc-33) - 10000
				// Convert to feet to match the rest of Decoded.Altitude.
				// 1 meter = 3.28084 feet.
				d.Altitude = int(float64(altM)*3.28084 + 0.5)
				c = c[i+1:]
				altDecoded = true
			}
		}
		if !altDecoded {
			// No valid altitude prefix; trim leading control bytes including
			// Mic-E 0x1C-0x1F fix-status indicators (APRS101 §10.1.1).
			// Capture old-fix indicator before discarding.
			for len(c) > 0 && c[0] < 0x20 {
				if c[0] == 0x1E || c[0] == 0x1F {
					d.MicEFixOld = true
				}
				c = c[1:]
			}
		}
		d.Comment = sanitizeASCII(string(c))
	}
	d.isMicE = true
}

// decodeTelemetry parses the APRS telemetry data packet body (everything after
// the leading "T#"). Format per spec 1.0.1 §13.1:
//
//	NNN,aaa,aaa,aaa,aaa,aaa,bbbbbbbb[,comment]
//
// where NNN is sequence (0..999 or "MIC"), a* are analog channels (1..3 digits,
// 0..255 conventional but tracker software emits floats), and b* are 8 binary
// digits ('0' or '1'). Any of the trailing fields may be empty.
// decodeNMEA parses a raw NMEA-0183 sentence (DTI `$`) for position and,
// where available, course/speed/altitude. Supports the two forms commonly
// emitted by legacy/cheap GPS trackers per APRS101 §5: $GPRMC and $GPGGA.
// Other sentences ($GPGLL, $GPVTG, $GPWPL) were defined but rarely seen
// in modern APRS traffic; skip them.
//
// Format reminders (no checksum verification — we trust the bytes that
// got through TNC):
//
//	$GPRMC,hhmmss,A,llll.ll,N,yyyyy.yy,W,sss.s,ccc.c,ddmmyy,...*CS
//	$GPGGA,hhmmss,llll.ll,N,yyyyy.yy,W,fix,nn,h.h,alt,M,...*CS
func decodeNMEA(info string, d *Decoded) {
	// Verify NMEA-0183 checksum if present. Format: `<body>*HH` where HH
	// is the hex XOR of every byte between `$` and `*` (exclusive). RF
	// corruption is real — aprs.fi validates and drops bad ones, we do
	// the same so a garbled GPRMC doesn't get re-emitted to APRS-IS.
	// A missing `*HH` (legitimate per spec — checksum is optional in
	// APRS101) is accepted without check.
	if star := strings.IndexByte(info, '*'); star > 0 {
		if len(info) >= star+3 {
			want, err := strconv.ParseUint(info[star+1:star+3], 16, 8)
			if err == nil {
				var got byte
				// Skip leading `$` (not part of checksummed body).
				start := 0
				if len(info) > 0 && info[0] == '$' {
					start = 1
				}
				for i := start; i < star; i++ {
					got ^= info[i]
				}
				if got != byte(want) {
					return // bad checksum — drop frame
				}
			}
		}
		info = info[:star]
	}
	fields := strings.Split(info, ",")
	if len(fields) < 1 {
		return
	}
	switch fields[0] {
	case "$GPRMC":
		// fields: 0=$GPRMC 1=time 2=A/V(status) 3=lat 4=N/S 5=lon 6=E/W 7=knots 8=course
		if len(fields) < 9 || fields[2] != "A" {
			return // no fix
		}
		lat, ok := nmeaCoord(fields[3], fields[4], false)
		if !ok {
			return
		}
		lon, ok := nmeaCoord(fields[5], fields[6], true)
		if !ok {
			return
		}
		d.Lat = &lat
		d.Lon = &lon
		if knots, err := strconv.ParseFloat(fields[7], 64); err == nil && knots > 0 {
			d.Speed = int(knots*1.15078 + 0.5) // knots → mph
		}
		if course, err := strconv.ParseFloat(fields[8], 64); err == nil && course >= 1 && course <= 360 {
			d.Course = int(course + 0.5)
		}
	case "$GPGGA":
		// fields: 0=$GPGGA 1=time 2=lat 3=N/S 4=lon 5=E/W 6=fix 7=sats 8=hdop 9=alt 10=M
		if len(fields) < 10 {
			return
		}
		if fields[6] == "0" || fields[6] == "" {
			return // no fix
		}
		lat, ok := nmeaCoord(fields[2], fields[3], false)
		if !ok {
			return
		}
		lon, ok := nmeaCoord(fields[4], fields[5], true)
		if !ok {
			return
		}
		d.Lat = &lat
		d.Lon = &lon
		if alt, err := strconv.ParseFloat(fields[9], 64); err == nil {
			d.Altitude = int(alt*3.28084 + 0.5) // meters → feet
		}
	}
}

// nmeaCoord parses an NMEA lat/lon field. lat form `DDMM.MM[MM]`; lon form
// `DDDMM.MM[MM]`. Hemisphere is N/S or E/W. Returns the decimal-degrees
// value and ok flag.
func nmeaCoord(val, hem string, isLon bool) (float64, bool) {
	if val == "" {
		return 0, false
	}
	dotIdx := strings.IndexByte(val, '.')
	if dotIdx < 3 || (isLon && dotIdx < 4) {
		return 0, false
	}
	degLen := dotIdx - 2
	deg, err := strconv.Atoi(val[:degLen])
	if err != nil {
		return 0, false
	}
	min, err := strconv.ParseFloat(val[degLen:], 64)
	if err != nil {
		return 0, false
	}
	v := float64(deg) + min/60.0
	if hem == "S" || hem == "W" {
		v = -v
	}
	return v, true
}

func decodeTelemetry(body string, d *Decoded) {
	parts := strings.SplitN(body, ",", 8) // seq + 5 analog + bits + (rest = comment)
	if len(parts) < 2 {
		return
	}
	d.IsTelemetry = true
	if seq, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil {
		d.TelemSeq = seq
	}
	for i := 0; i < 5 && i+1 < len(parts); i++ {
		s := strings.TrimSpace(parts[i+1])
		if s == "" {
			continue
		}
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			d.TelemAnalog[i] = v
		}
	}
	if len(parts) >= 7 {
		bits := strings.TrimSpace(parts[6])
		for i := 0; i < len(bits) && i < 8; i++ {
			d.TelemBits[i] = bits[i] == '1'
		}
	}
	if len(parts) == 8 {
		d.Comment = strings.TrimSpace(parts[7])
	}
}

func indexByteBounded(b []byte, c byte, max int) int {
	if max > len(b) {
		max = len(b)
	}
	for i := 0; i < max; i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// ---- Message ----

func decodeMessage(rest string, d *Decoded) {
	if len(rest) < 10 || rest[9] != ':' {
		return
	}
	to := strings.TrimRight(rest[:9], " ")
	body := rest[10:]
	// Telemetry config messages (PARM./UNIT./EQNS./BITS.) look like 1:1
	// messages on the wire but describe a station's telemetry channels.
	// Detected before setting IsMessage so the UI doesn't surface them
	// in the chat thread — they belong on the station-detail page where
	// they can be applied to subsequent T# data.
	if tc := parseTelemConfig(body); tc != nil {
		d.TelemConfig = tc
		d.MsgTo = to
		return
	}
	d.IsMessage = true
	d.MsgTo = to
	// Bulletin group: `BLN` + identifier byte + optional 0-5 chars group.
	// Examples: "BLN1ARES" → group "ARES"; "BLNAWX" → group "WX"; "BLN4"
	// → no group. APRS101 §14.
	if len(to) >= 4 && strings.HasPrefix(strings.ToUpper(to), "BLN") {
		if len(to) > 4 {
			d.BulletinGroup = strings.TrimRight(to[4:], " ")
		}
	}
	if strings.HasPrefix(body, "ack") && len(body) > 3 {
		d.IsAck = true
		d.AckedID, d.ReplyAckID = splitAckBody(body[3:])
		return
	}
	if strings.HasPrefix(body, "rej") && len(body) > 3 {
		d.IsRej = true
		d.AckedID, d.ReplyAckID = splitAckBody(body[3:])
		return
	}
	if i := strings.IndexByte(body, '{'); i >= 0 {
		d.MsgBody = body[:i]
		tail := strings.TrimRight(body[i+1:], "\r\n")
		// APRS 1.1 reply-ack: tail is `MM` (no piggyback) or `MM}AA` (piggyback
		// ack of peer's earlier msgID AA). Per aprs11/replyacks.txt the `}` is
		// the chosen separator because it isn't a valid base-91 byte.
		if j := strings.IndexByte(tail, '}'); j >= 0 {
			d.MsgID = sanitizeMsgID(tail[:j])
			d.ReplyAckID = sanitizeMsgID(tail[j+1:])
		} else {
			d.MsgID = sanitizeMsgID(tail)
		}
	} else {
		d.MsgBody = strings.TrimRight(body, "\r\n")
	}
}

// sanitizeMsgID strips anything outside printable ASCII from an APRS message
// ID. Real radios sometimes emit garbage in the msgID field (stuck buffer,
// broken firmware) which would otherwise render as � in the UI. APRS message
// IDs are spec'd as printable ASCII (alphanumeric per APRS 1.0, base-91
// printable per APRS 1.1 reply-ack form), so this is always safe.
// looksLikeTimestamp reports whether `s` (7 bytes) is a valid APRS101 §6
// timestamp: 6 digits followed by a suffix in {z,h,/}. Used to distinguish
// `/091245z…` (position-with-timestamp) from legacy `/9…` GPS-port DTIs.
//
// Also accepts any uppercase ASCII letter as a suffix — real-world clients
// (APRSIS32, Microsat WX3in1, etc.) emit non-standard suffixes like `I`,
// and the downstream position parser fails cleanly if what follows isn't a
// valid position, so leniency here doesn't false-positive on legacy `/N…`.
func looksLikeTimestamp(s string) bool {
	if len(s) != 7 {
		return false
	}
	c := s[6]
	if c != 'z' && c != 'h' && c != '/' && !(c >= 'A' && c <= 'Z') {
		return false
	}
	return isAllDigits(s[:6])
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func sanitizeMsgID(s string) string {
	s = strings.TrimRight(s, "\r\n")
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x20 && s[i] < 0x7F {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// splitAckBody splits the body after "ack" or "rej" into the acked-msg-ID
// and an optional piggyback free-ACK ID per APRS 1.1 reply-acks. Form is
// `MM` or `MM}AA` — `}` is the separator (chosen because it isn't a valid
// base-91 byte). Both halves are sanitized.
func splitAckBody(s string) (acked, piggyback string) {
	s = strings.TrimRight(s, "\r\n")
	if j := strings.IndexByte(s, '}'); j >= 0 {
		return sanitizeMsgID(s[:j]), sanitizeMsgID(s[j+1:])
	}
	return sanitizeMsgID(s), ""
}

// ---- Symbol table validators (verbatim from aprx) ----

func validSymTableCompressed(c byte) bool {
	return c == '/' || c == '\\' ||
		(c >= 0x41 && c <= 0x5A) || // A-Z
		(c >= 0x61 && c <= 0x6A) // a-j
}

func validSymTableUncompressed(c byte) bool {
	return c == '/' || c == '\\' ||
		(c >= 0x30 && c <= 0x39) || // 0-9
		(c >= 0x41 && c <= 0x5A) // A-Z
}

// sanitizeASCII keeps only printable ASCII (0x20-0x7E). Use for fields
// that MUST be strict ASCII — message IDs, callsigns, structured fields.
// For free-form display text (comments, status, message bodies) use
// sanitizeText below, which preserves Latin-1 by converting to UTF-8 so
// real-world transmitters' degree symbols, fancy quotes, etc. render
// correctly in the web UI instead of as the U+FFFD replacement char.
func sanitizeASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c < 0x7F {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// sanitizeText cleans a free-form display field: strips control chars,
// preserves printable ASCII (0x20-0x7E), passes through already-valid
// UTF-8 sequences verbatim (modern clients like APRSdroid emit UTF-8
// directly, and some upstream iGates re-encode Latin-1→UTF-8 before we
// see the bytes), and falls back to Latin-1→UTF-8 conversion for stray
// 8-bit bytes (0xA0-0xFF) so ° · ± etc. from older trackers still render.
//
// 0x80-0x9F (Latin-1 C1 controls) are dropped when seen as raw bytes:
// they're either control chars or codepage glyphs that vary by system.
func sanitizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			if c >= 0x20 && c != 0x7F {
				b.WriteByte(c)
			}
			i++
			continue
		}
		// High byte: try to decode as a UTF-8 sequence first. If it's a
		// valid multi-byte rune, copy the bytes through unchanged so we
		// don't double-encode chars that are already correct UTF-8.
		if r, size := utf8.DecodeRuneInString(s[i:]); r != utf8.RuneError && size > 1 {
			// Drop runes that decode to control chars (C0 / DEL / C1).
			if !(r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F)) {
				b.WriteString(s[i : i+size])
			}
			i += size
			continue
		}
		// Invalid UTF-8 start or lone continuation byte: treat as Latin-1.
		if c >= 0xA0 {
			b.WriteByte(0xC0 | (c >> 6))
			b.WriteByte(0x80 | (c & 0x3F))
		}
		// 0x80-0x9F as raw bytes: drop (C1 controls / codepage glyphs).
		i++
	}
	return b.String()
}
