package api

import (
	"context"
	"errors"
	"strings"
)

// ErrNoCredentials is returned when a client has no TokenSource.
var ErrNoCredentials = errors.New("api: no credentials")

// TokenSource is how the client gets the bearer it sends, and how it tells the
// source the API refused it. Two implementations exist:
//
//   - StaticKey, for --key and LISTO_API_KEY (here);
//   - the login lane's grant source, which holds the access token and the
//     rotating refresh token and refreshes under config.Store.WithLock.
//
// The client calls Token before every request. When the API answers 401 it
// calls Rejected(token) with the bearer it just sent; a source that can get a
// fresh token (a refresh) does so and returns retry=true, and the client sends
// the request ONCE more with a new Token() and the same Idempotency-Key. A
// source that cannot (a key) returns false and the 401 surfaces as an *Error.
// Rejected returning an error stops the request with that error (for example the
// login lane's "Session ended. Run listo login.").
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Rejected(ctx context.Context, token string) (retry bool, err error)
}

// StaticKey is a TokenSource for an API key (lst_test_... or lst_live_...).
type StaticKey string

func (k StaticKey) Token(context.Context) (string, error)          { return string(k), nil }
func (k StaticKey) Rejected(context.Context, string) (bool, error) { return false, nil }

// Key prefixes: the prefix decides the mode of a key (a test key dispatches
// simulated couriers and moves no money).
const (
	TestKeyPrefix = "lst_test_"
	LiveKeyPrefix = "lst_live_"
)

// KeyMode returns "test" or "live" for an API key by its prefix, and false for
// anything else.
func KeyMode(key string) (string, bool) {
	switch {
	case strings.HasPrefix(key, TestKeyPrefix):
		return "test", true
	case strings.HasPrefix(key, LiveKeyPrefix):
		return "live", true
	}
	return "", false
}
