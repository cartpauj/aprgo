// Package gps reads a station's live position from a local NMEA-0183 serial
// receiver or from the gpsd daemon, and exposes the freshest fix to the beacon
// scheduler so beacons can transmit a GPS position instead of a fixed one.
//
// The NMEA decoder here is deliberately talker-ID agnostic: it keys on the
// 3-char *sentence type* (RMC/GGA/...) and ignores the 2-char talker prefix,
// so $GPRMC (GPS-only), $GNRMC (multi-GNSS combined), $GLRMC (GLONASS), etc.
// all decode identically. Hardcoding "GP" silently breaks every modern
// multi-constellation receiver, which emit $GN....
package gps

import (
	"strconv"
	"strings"
)

// sentenceData is the union of fields we extract from a single NMEA sentence.
// The have* flags distinguish "field absent / empty" from "field present and
// zero" — critical because a receiver with no fix still emits well-formed
// RMC/GGA with empty position and zero counts.
type sentenceData struct {
	typ string // 3-char sentence type, e.g. "RMC", "GGA"

	havePos  bool
	lat, lon float64

	haveStatus bool
	valid      bool // RMC status A (true) vs V (false)

	haveQuality bool
	quality     int // GGA fix quality: 0 = no fix, >=1 = fix

	haveSats bool
	sats     int

	haveHDOP bool
	hdop     float64

	haveSpeed  bool
	speedKnots float64

	haveTrack bool
	track     float64

	haveMode bool
	mode     int // GSA fix type: 1 = none, 2 = 2D, 3 = 3D
}

// knownSentences are the sentence types we recognise. Used both for parsing
// dispatch and for device detection (a device emitting several checksum-valid
// known sentences is a GPS, even before it has a fix — TXT/GSV/GSA appear
// during acquisition).
var knownSentences = map[string]bool{
	"RMC": true, "GGA": true, "GLL": true, "VTG": true,
	"GSA": true, "GSV": true, "TXT": true, "ZDA": true,
}

// validChecksum reports whether line carries a valid NMEA checksum: the 8-bit
// XOR of every byte strictly between '$' and '*', compared to the two hex
// digits after '*'. This is the reliable "real GPS vs serial noise / wrong
// baud" test — random bytes essentially never pass.
func validChecksum(line string) bool {
	line = strings.TrimSpace(line)
	if len(line) < 4 || line[0] != '$' {
		return false
	}
	star := strings.LastIndexByte(line, '*')
	if star < 1 || star+3 > len(line) {
		return false
	}
	var cs byte
	for i := 1; i < star; i++ {
		cs ^= line[i]
	}
	want, err := strconv.ParseUint(line[star+1:star+3], 16, 8)
	return err == nil && byte(want) == cs
}

// sentenceType returns the 3-char type (talker prefix stripped) of a standard
// NMEA sentence, or "" for proprietary ($P...) or malformed lines. Assumes a
// leading '$' and a 5-char "ttSSS" token (2 talker + 3 type).
func sentenceType(line string) string {
	if len(line) < 6 || line[0] != '$' {
		return ""
	}
	end := strings.IndexByte(line, ',')
	if end < 0 {
		end = strings.IndexByte(line, '*')
	}
	if end < 0 {
		return ""
	}
	tok := line[1:end]
	if len(tok) != 5 { // standard = 2-char talker + 3-char type; proprietary differs
		return ""
	}
	return tok[2:]
}

// parseCoord decodes an NMEA "(d)ddmm.mmmm" + hemisphere pair into signed
// decimal degrees. degDigits is 2 for latitude, 3 for longitude.
func parseCoord(val, hemi string, degDigits int) (float64, bool) {
	if val == "" || hemi == "" || len(val) < degDigits+2 {
		return 0, false
	}
	deg, err := strconv.ParseFloat(val[:degDigits], 64)
	if err != nil {
		return 0, false
	}
	min, err := strconv.ParseFloat(val[degDigits:], 64)
	if err != nil {
		return 0, false
	}
	d := deg + min/60.0
	switch hemi {
	case "N", "E":
	case "S", "W":
		d = -d
	default:
		return 0, false
	}
	return d, true
}

// parseSentence validates the checksum, dispatches on the sentence type, and
// returns whatever fields it could extract. ok is false for lines that fail
// the checksum or aren't a recognised sentence type — those don't count as
// GPS evidence.
func parseSentence(line string) (sentenceData, bool) {
	if !validChecksum(line) {
		return sentenceData{}, false
	}
	typ := sentenceType(line)
	if typ == "" || !knownSentences[typ] {
		return sentenceData{}, false
	}
	d := sentenceData{typ: typ}
	star := strings.LastIndexByte(line, '*')
	f := strings.Split(line[1:star], ",") // f[0] = "ttSSS"

	switch typ {
	case "RMC":
		// $--RMC,time,status,lat,N/S,lon,E/W,SOG(kn),COG,date,...
		if len(f) < 7 {
			return d, true
		}
		d.haveStatus = true
		d.valid = f[2] == "A"
		if lat, ok := parseCoord(f[3], f[4], 2); ok {
			if lon, ok := parseCoord(f[5], f[6], 3); ok {
				d.lat, d.lon, d.havePos = lat, lon, true
			}
		}
		if len(f) > 7 {
			if v, err := strconv.ParseFloat(f[7], 64); err == nil {
				d.speedKnots, d.haveSpeed = v, true
			}
		}
		if len(f) > 8 {
			if v, err := strconv.ParseFloat(f[8], 64); err == nil {
				d.track, d.haveTrack = v, true
			}
		}
	case "GGA":
		// $--GGA,time,lat,N/S,lon,E/W,quality,numSat,HDOP,alt,...
		if len(f) < 9 {
			return d, true
		}
		if v, err := strconv.Atoi(f[6]); err == nil {
			d.quality, d.haveQuality = v, true
		}
		if v, err := strconv.Atoi(f[7]); err == nil {
			d.sats, d.haveSats = v, true
		}
		if v, err := strconv.ParseFloat(f[8], 64); err == nil {
			d.hdop, d.haveHDOP = v, true
		}
		if lat, ok := parseCoord(f[2], f[3], 2); ok {
			if lon, ok := parseCoord(f[4], f[5], 3); ok {
				d.lat, d.lon, d.havePos = lat, lon, true
			}
		}
	case "GLL":
		// $--GLL,lat,N/S,lon,E/W,time,status,...
		if len(f) < 7 {
			return d, true
		}
		d.haveStatus = true
		d.valid = f[6] == "A"
		if lat, ok := parseCoord(f[1], f[2], 2); ok {
			if lon, ok := parseCoord(f[3], f[4], 3); ok {
				d.lat, d.lon, d.havePos = lat, lon, true
			}
		}
	case "GSA":
		// $--GSA,mode,fixtype,... — field 2 is 1=no fix, 2=2D, 3=3D.
		if len(f) < 3 {
			return d, true
		}
		if v, err := strconv.Atoi(f[2]); err == nil {
			d.mode, d.haveMode = v, true
		}
	}
	return d, true
}

const knotsToMS = 0.514444
