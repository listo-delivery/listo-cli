// Package logs is `listo logs tail`: the account's /v1/ request log, live, over
// the developer's socket (the `request_log` frame),
// with the same reconnect catch-up as `listen` (through GET /v1/request_logs).
package logs

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/cable"
)

// Endpoint labels of the two reads this CLI makes ITSELF while it runs: `listen`
// and `logs tail` catch up through GET /v1/events and GET /v1/request_logs, and
// each of those is a /v1/ call the ledger records and the socket then streams
// back. The strings are the `controller#action` labels in
// api/v1/request_log_serializer.rb (and Api::BodyCapture::BODILESS_ENDPOINTS).
var catchUpEndpoints = map[string]bool{
	"api/v1/events#index":       true,
	"api/v1/request_logs#index": true,
}

// KeyPrefixLength is how many characters of a key the server shows as a row's
// `api_key_prefix` (PartnerApiKey::PREFIX_LENGTH).
const KeyPrefixLength = 12

// Filter decides which rows are printed.
type Filter struct {
	// Status is "", "failed" (status 400 or above) or "succeeded" (below 400).
	Status string
	// Endpoint, when set, keeps only rows of that exact endpoint label.
	Endpoint string
	// All shows this CLI's own catch-up reads too.
	All bool
	// Token is the credential this run uses. It decides which rows are "ours".
	Token string
}

// ownPrefix is the `api_key_prefix` a row written by THIS run's credential
// carries: the first characters of a key, and none at all for a CLI sign-in,
// whose request-log rows carry a null prefix (measured, play-testing guide:
// the grant's hidden key is not a visible key).
func (f Filter) ownPrefix() (prefix string, hasOne bool) {
	if _, isKey := api.KeyMode(f.Token); isKey && len(f.Token) >= KeyPrefixLength {
		return f.Token[:KeyPrefixLength], true
	}
	return "", false
}

// IsOwnCatchUp reports whether the row is one of this CLI's own catch-up reads.
//
// The rule is the ENDPOINT, plus the prefix where the credential has one: with a
// sign-in the rows carry no prefix, so a row of those two endpoints with a null
// prefix is ours; with a key, it is ours only when its prefix is that key's, so
// another integration's own reads of the same lists, made with a different key,
// still show. (A second CLI signed in as the same account is indistinguishable
// from this one, and its rows are hidden too: the endpoint says what it is and
// nothing says whose.)
func (f Filter) IsOwnCatchUp(l api.RequestLog) bool {
	if !catchUpEndpoints[l.Endpoint] {
		return false
	}
	prefix, hasOne := f.ownPrefix()
	if hasOne {
		return l.APIKeyPrefix != nil && *l.APIKeyPrefix == prefix
	}
	return l.APIKeyPrefix == nil
}

// Show reports whether a row is printed.
func (f Filter) Show(l api.RequestLog) bool {
	switch f.Status {
	case "failed":
		if l.Status < 400 {
			return false
		}
	case "succeeded":
		if l.Status >= 400 {
			return false
		}
	}
	if f.Endpoint != "" && l.Endpoint != f.Endpoint {
		return false
	}
	if !f.All && f.IsOwnCatchUp(l) {
		return false
	}
	return true
}

type frame struct {
	Type string          `json:"type"`
	Log  json.RawMessage `json:"log"`
}

// DecodeFrame reads a data frame's payload; ok is false for anything that is not
// a request_log frame.
func DecodeFrame(payload json.RawMessage) (api.RequestLog, bool) {
	var f frame
	if err := json.Unmarshal(payload, &f); err != nil || f.Type != "request_log" || len(f.Log) == 0 {
		return api.RequestLog{}, false
	}
	var l api.RequestLog
	if err := json.Unmarshal(f.Log, &l); err != nil || l.ID == "" {
		return api.RequestLog{}, false
	}
	return l, true
}

func created(l api.RequestLog) time.Time {
	t, err := time.Parse(time.RFC3339, l.CreatedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Session is one `logs tail` run.
type Session struct {
	Client *api.Client
	Cable  cable.Options
	Filter Filter

	// Now is the clock the catch-up floor uses; default time.Now (tests set it).
	Now func() time.Time

	Ready   func()
	Row     func(api.RequestLog)
	Notice  func(cable.Notice)
	CatchUp func(missed int, err error)
}

// Run tails until the context ends, the session ends or the server closes the
// socket.
func (s *Session) Run(ctx context.Context) error {
	feed := &cable.Feed[api.RequestLog]{
		Decode: DecodeFrame,
		Key:    func(l api.RequestLog) (string, time.Time) { return l.ID, created(l) },
		Fetch: func(ctx context.Context, after time.Time, each func(api.RequestLog) (bool, error)) error {
			q := url.Values{"created_after": {after.UTC().Format(time.RFC3339)}}
			return s.Client.EachRequestLog(ctx, q, each)
		},
		Handle: func(_ context.Context, l api.RequestLog) {
			if s.Filter.Show(l) {
				s.Row(l)
			}
		},
		CatchUp: s.CatchUp,
		Now:     s.Now,
	}
	cb := cable.Callbacks{
		Notice: s.Notice,
		Subscribed: func(first bool) {
			if first && s.Ready != nil {
				s.Ready()
			}
		},
	}
	opts := s.Cable
	if opts.Probe == nil && s.Client != nil {
		opts.Probe = func(ctx context.Context) error { _, err := s.Client.Ping(ctx); return err }
	}
	return feed.Run(ctx, cable.New(opts), cb)
}
