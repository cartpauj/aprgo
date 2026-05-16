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
	"regexp"
	"strconv"
	"strings"
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
	Frequency  string  // e.g. "144.390 MHz", empty if unknown
	Status     string  // Mic-E status string, e.g. "In Service"
	IsMessage  bool
	MsgTo      string
	MsgBody    string
	MsgID      string
	IsAck      bool
	AckedID    string

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
		// position with timestamp; skip 7-char timestamp
		if len(info) >= 8 {
			decodeUncompressedOrCompressed(info[8:], &d)
		}
	case ';':
		// object: ;NAME9NNN*/_ddmm.mmN/dddmm.mmW_...
		if len(info) > 11 {
			decodeUncompressedOrCompressed(info[11:], &d)
		}
	case ')':
		// item: )NAME!_pos...   or   )NAME*_pos...
		// Item-packet format: `)NAME!data` (alive) or `)NAME_data` (killed)
		if i := strings.IndexAny(info[1:], "!_"); i >= 0 && len(info) > i+2 {
			decodeUncompressedOrCompressed(info[i+2:], &d)
		}
	case '`', '\'':
		decodeMicE(info, dest, &d)
	case ':':
		decodeMessage(info[1:], &d)
	case '>':
		d.Comment = strings.TrimRight(info[1:], "\r\n")
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
			if course >= 0 && course <= 360 {
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

	d.Comment = c
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
	csRegex   = regexp.MustCompile(`^([0-9]{3})/([0-9]{3})\b`)
	freqRegex = regexp.MustCompile(`\b(\d{2,3}\.\d{3,4})\s?MHz\b`)
)

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
	// Position ambiguity: spaces become specific digits per APRS spec
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
	d.Lat = &lat
	d.Lon = &lon
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
		if course >= 0 && course < 360 {
			d.Course = course
		} else {
			d.Course = -1
		}
	}

	// Mic-E "extended status" follows the symbol bytes. Format examples:
	//   `"Cv}<comment>      (typical: backtick + 2-3 bytes + '}')
	//   ''Bw}<comment>
	//   <0x1d>><comment>    (status byte + comment, no '}')
	// Drop the extended block if it ends with '}' within ~8 bytes; else
	// just trim leading control bytes.
	if len(body) > 8 {
		c := body[8:]
		if i := indexByteBounded(c, '}', 8); i >= 0 {
			c = c[i+1:]
		} else {
			for len(c) > 0 && c[0] < 0x20 {
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
	d.IsMessage = true
	d.MsgTo = to
	if strings.HasPrefix(body, "ack") && len(body) > 3 {
		d.IsAck = true
		d.AckedID = strings.TrimRight(body[3:], "\r\n")
		return
	}
	if strings.HasPrefix(body, "rej") && len(body) > 3 {
		d.IsAck = true
		d.AckedID = strings.TrimRight(body[3:], "\r\n")
		return
	}
	if i := strings.LastIndex(body, "{"); i >= 0 {
		d.MsgBody = body[:i]
		d.MsgID = strings.TrimRight(body[i+1:], "\r\n")
	} else {
		d.MsgBody = strings.TrimRight(body, "\r\n")
	}
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

func sanitizeASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 32 && r < 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}
