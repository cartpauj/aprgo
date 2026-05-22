package aprs

import "strings"

// PathHop is one entry in a packet's via path.
//
//   AX.25-level hops (real digipeaters) have IsQConstruct == false.
//   The optional Used flag is set when the hop was marked '*' meaning
//   that digipeater actually relayed this packet.
//
//   APRS-IS adds q-constructs (qAR, qAC, qAO, qAS, qAS, qAX, qAr, qAR…)
//   plus an "I-gate-callsign" hop after them. Those are flagged with
//   IsQConstruct so the UI can render them as a separate badge instead
//   of mixing them into the AX.25 digipeater chain.
type PathHop struct {
	Call         string // e.g. "WIDE1-1", "N0CALL-10", or "qAR"
	Used         bool   // '*' marker stripped from Call
	IsQConstruct bool   // true for qAR/qAC/qAO/qAS/qAx (and the iGate call following it)
}

// PathSummary describes a packet's full via path in structured form.
type PathSummary struct {
	Hops        []PathHop // in order, including q-construct hops at end
	QConstruct  string    // "qAR", "qAC", etc., empty if none present
	IGateCall   string    // the call that follows the q-construct on IS-side
	DigiCount   int       // number of AX.25-layer hops that actually relayed
	DigiCapable int       // number of AX.25-layer hops in the path (used + remaining)
}

// ParsePath splits an AX.25 path string ("WIDE1*,WIDE2-1,qAR,N0CALL-10")
// into structured hops. Empty / whitespace-only input returns an empty
// PathSummary.
func ParsePath(path string) PathSummary {
	ps := PathSummary{}
	path = strings.TrimSpace(path)
	if path == "" {
		return ps
	}
	parts := strings.Split(path, ",")
	qFound := false
	for _, raw := range parts {
		hop := PathHop{Call: strings.TrimSpace(raw)}
		if hop.Call == "" {
			continue
		}
		if strings.HasSuffix(hop.Call, "*") {
			hop.Used = true
			hop.Call = strings.TrimSuffix(hop.Call, "*")
		}
		// Detect q-construct ("qAx" where x is a single letter). Once we
		// see one, every subsequent hop is also IS-layer metadata.
		if qFound {
			hop.IsQConstruct = true
			if ps.IGateCall == "" {
				ps.IGateCall = hop.Call
			}
		} else if isQConstruct(hop.Call) {
			hop.IsQConstruct = true
			qFound = true
			ps.QConstruct = hop.Call
		} else {
			ps.DigiCapable++
			if hop.Used {
				ps.DigiCount++
			}
		}
		ps.Hops = append(ps.Hops, hop)
	}
	return ps
}

// isQConstruct reports whether `s` matches an APRS-IS q-construct token:
// `qAR`, `qAC`, `qAO`, `qAS`, `qAX`, `qAr`, `qAU`, `qAI`, `qAa`, etc.
// All are 3 chars starting with `q` and a capital A (or lowercase a).
func isQConstruct(s string) bool {
	if len(s) != 3 {
		return false
	}
	if s[0] != 'q' {
		return false
	}
	if s[1] != 'A' && s[1] != 'a' {
		return false
	}
	c := s[2]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// HopSummary returns a short human-readable description of the digipeat
// chain. Examples: "Direct" (no digis), "1 digi", "2 digi", "Via APRS-IS"
// (only q-construct present, no AX.25 hops).
func (p PathSummary) HopSummary() string {
	if p.DigiCapable == 0 && p.QConstruct != "" {
		return "Via APRS-IS"
	}
	if p.DigiCount == 0 && p.DigiCapable == 0 {
		return "Direct"
	}
	if p.DigiCount == 0 {
		return "Direct (path unused)"
	}
	if p.DigiCount == 1 {
		return "1 digi"
	}
	// Pluralize plainly.
	return itoa(p.DigiCount) + " digis"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [11]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
