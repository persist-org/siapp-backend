package patrol

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// QR image formats of GET /admin/zones/{id}/qr.
const (
	FormatPNG = "png"
	FormatSVG = "svg"
)

// qrPayloadVersion is the version field of the payload, so a later layout can be told apart from
// the one printed on stickers today.
const qrPayloadVersion = 1

// pngSize is the side of the PNG in pixels: big enough to print a sticker sharply.
const pngSize = 512

// Payload is what the zone's QR code holds: PERSIST-ZONE:1:{organizationId}:{zoneId}:{qrSecret}.
//
// ⚠ It is static, unlike the attendance QR (which rotates), because it is printed on a sticker
// and stuck at the post: a code that changed every minute could not be. What keeps a static code
// from being a free pass is not the code but what the server does with a scan:
//   - the geofence (Phase 10.2) rejects a scan made away from the zone, whatever the code says, so
//     a photo of the sticker does nothing from the couch;
//   - the organization id in the payload stops a sticker of one organization from opening a zone
//     of another;
//   - when a sticker leaks, the admin rotates the secret (POST /admin/zones/{id}/rotate-qr) and
//     every printed one stops working at once.
func Payload(organizationID, zoneID int64, secret string) string {
	return fmt.Sprintf("PERSIST-ZONE:%d:%d:%d:%s", qrPayloadVersion, organizationID, zoneID, secret)
}

// newSecret returns a QR secret: 16 random bytes in base64url, which is 22 characters.
func newSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate qr secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// renderQR returns the image of payload in the format, and its Content-Type.
func renderQR(payload, format string) (body []byte, contentType string, err error) {
	switch format {
	case FormatPNG:
		png, err := qrcode.Encode(payload, qrcode.Medium, pngSize)
		if err != nil {
			return nil, "", fmt.Errorf("render qr png: %w", err)
		}
		return png, "image/png", nil
	case FormatSVG:
		code, err := qrcode.New(payload, qrcode.Medium)
		if err != nil {
			return nil, "", fmt.Errorf("render qr svg: %w", err)
		}
		return svgOf(code.Bitmap()), "image/svg+xml", nil
	default:
		return nil, "", fmt.Errorf("render qr: unknown format %q", format)
	}
}

// svgOf draws the bitmap (true is a dark module, the quiet zone included) as one path, so the
// file stays small however many modules the code has.
func svgOf(bitmap [][]bool) []byte {
	n := len(bitmap)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ` + strconv.Itoa(n) + ` ` + strconv.Itoa(n) +
		`" shape-rendering="crispEdges"><rect width="100%" height="100%" fill="#fff"/><path fill="#000" d="`)
	for y, row := range bitmap {
		for x := 0; x < len(row); x++ {
			if !row[x] {
				continue
			}
			// A run of dark modules in a row is one rectangle.
			start := x
			for x+1 < len(row) && row[x+1] {
				x++
			}
			b.WriteString("M" + strconv.Itoa(start) + " " + strconv.Itoa(y) + "h" + strconv.Itoa(x-start+1) + "v1h-" + strconv.Itoa(x-start+1) + "z")
		}
	}
	b.WriteString(`"/></svg>`)
	return []byte(b.String())
}

// fileName is the download name of a zone's QR: "qr-zona-<name>.<ext>", with the name reduced to
// lowercase letters, digits and hyphens so it is safe in a header and on any file system.
func fileName(zoneName, format string) string {
	var slug strings.Builder
	dash := true // no leading hyphen
	for _, r := range strings.ToLower(zoneName) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			slug.WriteRune(r)
			dash = false
		case !dash:
			slug.WriteByte('-')
			dash = true
		}
	}
	name := strings.TrimRight(slug.String(), "-")
	if len(name) > 60 {
		name = strings.TrimRight(name[:60], "-")
	}
	if name == "" {
		name = "zona"
	}
	return "qr-zona-" + name + "." + format
}
