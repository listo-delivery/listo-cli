// Package signature is the X-Listo-Signature scheme of Listo's webhooks:
//
//	X-Listo-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<raw body>">
//
// `listen` signs every event it forwards with the computer's session secret
// (Sign), and the same package verifies (Verify) so the CLI's own tests, and a
// developer reading this code, see the whole contract in one place. The body is
// ALWAYS the raw bytes about to go on the wire, never a parsed and re-encoded
// value: re-encoding changes the bytes and the verifier checks the bytes.
//
// The server signs with the same scheme, and a shared set of test vectors holds
// this package equal to it and to the three SDKs.
package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"time"
)

// Header is the name of the header the signature travels in.
const Header = "X-Listo-Signature"

// DefaultTolerance is the replay window, five minutes in both directions.
const DefaultTolerance = 5 * time.Minute

// ErrBlankSecret is returned by Sign: an HMAC keyed on "" is a signature
// anybody can compute, so a header is never produced from one.
var ErrBlankSecret = errors.New("signature: a signing secret is required")

var headerPattern = regexp.MustCompile(`\At=(\d+),v1=([0-9a-f]+)\z`)

func digest(t int64, body []byte, secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t, 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return mac.Sum(nil)
}

// Sign returns the header value for body at instant now.
func Sign(secret string, body []byte, now time.Time) (string, error) {
	if secret == "" {
		return "", ErrBlankSecret
	}
	t := now.Unix()
	return "t=" + strconv.FormatInt(t, 10) + ",v1=" + hex.EncodeToString(digest(t, body, secret)), nil
}

// Verify reports whether header is a well-formed signature over body, made
// within tolerance of now, under the secret. Like the server's verifier it
// fails closed on everything and never panics: a malformed header is false.
// A tolerance of zero or less means DefaultTolerance.
func Verify(secret string, body []byte, header string, now time.Time, tolerance time.Duration) bool {
	return VerifyAny([]string{secret}, body, header, now, tolerance)
}

// VerifyAny is Verify over several secrets (a rotation holds two active ones);
// blank secrets are ignored and no usable secret is false.
func VerifyAny(secrets []string, body []byte, header string, now time.Time, tolerance time.Duration) bool {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	m := headerPattern.FindStringSubmatch(header)
	if m == nil {
		return false
	}
	t, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return false
	}
	skew := now.Unix() - t
	if skew < 0 {
		skew = -skew
	}
	if skew > int64(tolerance/time.Second) {
		return false
	}
	given, err := hex.DecodeString(m[2])
	if err != nil {
		return false
	}
	ok := false
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if hmac.Equal(digest(t, body, s), given) {
			ok = true
		}
	}
	return ok
}
