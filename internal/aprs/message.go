package aprs

import "strings"

// MessageInfo builds the info field for an APRS message:
//
//	":CALLPAD :body{id"   (pad recipient to 9 chars)
func MessageInfo(toCall, body, id string) string {
	if len(toCall) > 9 {
		toCall = toCall[:9]
	}
	pad := toCall + strings.Repeat(" ", 9-len(toCall))
	out := ":" + pad + ":" + body
	if id != "" {
		out += "{" + id
	}
	return out
}

// AckInfo builds the info field for an APRS ack:
//
//	":CALLPAD :ackXXX"     (pad recipient to 9 chars, body = literal "ack"+id)
//
// Per APRS spec, acks themselves carry no msg-id and are not themselves acked.
func AckInfo(toCall, ackedID string) string {
	if len(toCall) > 9 {
		toCall = toCall[:9]
	}
	pad := toCall + strings.Repeat(" ", 9-len(toCall))
	return ":" + pad + ":ack" + ackedID
}
