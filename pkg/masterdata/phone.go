package masterdata

import (
	"regexp"
	"strings"
)

// e164 is the shape every stored phone number has: a plus, a country code that does not start
// with 0, and 8 to 15 digits in all.
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// shortCodes are the national emergency numbers of Indonesia. They are not E.164 (there is no
// country code), but they are exactly what a Polisi or Pemadam contact is, so they are stored as
// they are. Anything else short is refused: "1234" is a typo, not a number.
var shortCodes = map[string]bool{
	"110": true, // Polisi
	"112": true, // darurat umum
	"113": true, // Pemadam Kebakaran
	"115": true, // SAR / Basarnas
	"118": true, // ambulans
	"119": true, // ambulans / gawat darurat
	"123": true, // PLN
	"129": true, // Satpol PP / ketertiban
}

// normalizePhone returns the phone number as it is stored, or false when it is not a number.
//
// A person types what they dial, so the common local shapes are turned into E.164 first: spaces,
// dashes, dots and brackets are dropped, "0812…" becomes "+62812…" and "62812…" becomes
// "+62812…". What is left must be E.164 or a national emergency number.
func normalizePhone(raw string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && b.Len() == 0:
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
			// formatting only
		default:
			return "", false
		}
	}
	p := b.String()
	switch {
	case shortCodes[p]:
		return p, true
	case strings.HasPrefix(p, "+"):
	case strings.HasPrefix(p, "00"): // 0062…: the international call prefix
		p = "+" + p[2:]
	case strings.HasPrefix(p, "0"):
		p = "+62" + p[1:]
	case strings.HasPrefix(p, "62"):
		p = "+" + p
	default:
		return "", false
	}
	if !e164.MatchString(p) {
		return "", false
	}
	return p, true
}
