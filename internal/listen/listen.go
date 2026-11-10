// Package listen is `listo listen`: it receives the account's webhook events
// over the developer's live socket (internal/cable) and POSTs each to a URL on
// the developer's own computer, signed with a session secret of theirs, so a
// handler can be built with no public address.
//
// What it guarantees, in the words of section 4.1:
//
//   - The BODY forwarded is the frame's `body` string byte for byte: it is the
//     exact string the server's webhook sender POSTs, so the developer's verifier
//     sees what production would send. It is never parsed and re-encoded.
//   - The signature is `signature.Sign(secret, body, now)` with the computer's
//     own session secret (whsec_..., config.Store.WebhookSecret). The partner's
//     real webhook secret never leaves the server and is never used here.
//   - One line per event: time, type, id, what the local server answered and how
//     long it took. No retry: a developer who sees a 500 fixes the handler and
//     triggers again.
//   - A reconnect catches up what was written meanwhile (cable.Feed), except
//     courier positions, which the unfiltered event list leaves out on purpose.
package listen

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/cable"
	"github.com/listo-delivery/listo-cli/internal/signature"
	"github.com/listo-delivery/listo-cli/internal/version"
)

// Event is one webhook event: the exact body to forward and what the line needs.
type Event struct {
	ID      string
	Type    string
	Body    []byte // the bytes to POST, verbatim
	Created time.Time
}

// frame is the socket's `event` frame.
type frame struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	EventType string `json:"event_type"`
	Body      string `json:"body"`
}

// DecodeFrame reads a data frame's payload; ok is false for any frame that is
// not an event (a request_log frame, or something new the server adds).
func DecodeFrame(payload json.RawMessage) (Event, bool) {
	var f frame
	if err := json.Unmarshal(payload, &f); err != nil || f.Type != "event" || f.ID == "" {
		return Event{}, false
	}
	return Event{ID: f.ID, Type: f.EventType, Body: []byte(f.Body), Created: createdAt([]byte(f.Body))}, true
}

// FromAPI is an event as GET /v1/events listed it. Raw is the list item exactly
// as the server rendered it, which is the same envelope the webhook carries.
func FromAPI(e api.Event) Event {
	return Event{ID: e.ID, Type: e.Type, Body: []byte(e.Raw), Created: parseTime(e.CreatedAt)}
}

func createdAt(body []byte) time.Time {
	var env struct {
		CreatedAt string `json:"created_at"`
	}
	_ = json.Unmarshal(body, &env)
	return parseTime(env.CreatedAt)
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Result is what forwarding one event came to. Status is 0 when the URL could
// not be reached (Err says why).
type Result struct {
	Time     time.Time
	Type     string
	ID       string
	URL      string
	Status   int
	Duration time.Duration
	Err      error
}

// OK is a 2xx answer.
func (r Result) OK() bool { return r.Err == nil && r.Status >= 200 && r.Status < 300 }

// Forwarder POSTs events to one URL, signed.
type Forwarder struct {
	URL       string
	Secret    string
	HTTP      *http.Client // default: 30 second timeout, redirects not followed
	UserAgent string       // default "listo-cli/<version>"
	Now       func() time.Time
}

// UserAgentHeader is the User-Agent the forward carries (section 4.1 item 9).
func UserAgentHeader() string { return "listo-cli/" + version.Version }

// Forward sends ev once. It never retries and never returns an error: the
// outcome, including "could not connect", is the Result.
func (f *Forwarder) Forward(ctx context.Context, ev Event) Result {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	hc := f.HTTP
	if hc == nil {
		hc = &http.Client{
			Timeout: 30 * time.Second,
			// A 3xx is an answer to report, not a hop to follow with a body.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	ua := f.UserAgent
	if ua == "" {
		ua = UserAgentHeader()
	}

	res := Result{Time: now(), Type: ev.Type, ID: ev.ID, URL: f.URL}
	sig, err := signature.Sign(f.Secret, ev.Body, now())
	if err != nil {
		res.Err = err
		return res
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.URL, bytes.NewReader(ev.Body))
	if err != nil {
		res.Err = err
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.Header.Set(signature.Header, sig)

	start := time.Now()
	resp, err := hc.Do(req)
	res.Duration = time.Since(start)
	if err != nil {
		res.Err = err
		return res
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	res.Status = resp.StatusCode
	return res
}

// Session is one `listen` run.
type Session struct {
	Client    *api.Client
	Cable     cable.Options
	Forwarder *Forwarder
	// Types filters by event type; nil forwards everything.
	Types map[string]bool

	// Now is the clock the catch-up floor uses; default time.Now (tests set it).
	Now func() time.Time

	// Ready is called once, at the first confirmed subscription.
	Ready func()
	// Result is called for every forwarded event, in order.
	Result func(Result)
	// Notice passes the socket's reconnect and close notices on.
	Notice func(cable.Notice)
	// CatchUp reports a finished catch-up after a reconnect.
	CatchUp func(missed int, err error)
}

// Run listens until the context ends, the session ends or the server closes the
// socket. The error is cable.Client.Run's.
func (s *Session) Run(ctx context.Context) error {
	feed := &cable.Feed[Event]{
		Decode: DecodeFrame,
		Key:    func(e Event) (string, time.Time) { return e.ID, e.Created },
		Fetch: func(ctx context.Context, after time.Time, each func(Event) (bool, error)) error {
			q := url.Values{"created_after": {after.UTC().Format(time.RFC3339)}}
			return s.Client.EachEvent(ctx, q, func(e api.Event) (bool, error) { return each(FromAPI(e)) })
		},
		Handle: func(ctx context.Context, e Event) {
			if s.Types != nil && !s.Types[e.Type] {
				return
			}
			s.Result(s.Forwarder.Forward(ctx, e))
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
