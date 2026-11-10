// Package cable is the CLI's Action Cable client: the developer's live socket
// (`DeveloperChannel`, described in the partner developer docs) that `listo listen` and
// `listo logs tail` read.
//
// WHAT THE SERVER MEASURABLY DOES, which this client is written against (the
// play-testing guide, "The CLI's device grant and the developer's live socket"):
//
//   - A bad or revoked credential is NOT a failed handshake. The upgrade answers
//     101 and the very next frame is {"type":"disconnect","reason":"unauthorized",
//     "reconnect":false}. So the client reads the FRAME, never the status, to
//     learn it was refused. (A 401 or 403 on the handshake is read the same way,
//     for a proxy that answers one.)
//   - A bearer handshake carrying ANY Origin header is refused with 404, so this
//     client sends none: coder/websocket sets no Origin on a dial, and a test
//     asserts the request headers.
//   - Revoking a key or a grant closes its sockets right after the commit with
//     {"type":"disconnect","reason":"remote","reconnect":false}. That frame says
//     nothing about WHY, so it is never read as a clean end: the client checks
//     whether the credential still works (Options.Probe, else the TokenSource's
//     Rejected). A credential that no longer works ends the run as a session
//     ended (ErrSessionEnded) or a revoked key (ErrKeyRevoked); one that still
//     works is a server closing the socket for another reason, and the client
//     reconnects as from a dropped socket. Any other disconnect with
//     reconnect:false is a clean end.
//   - The server pings every 3 seconds, so a socket that has said nothing for 10
//     is dead.
//
// The wire is Action Cable's JSON protocol, subprotocol actioncable-v1-json:
// welcome, then {"command":"subscribe","identifier":"..."}, then
// confirm_subscription or reject_subscription, pings, message frames
// ({"identifier":...,"message":{...}}), and disconnect.
package cable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/version"
)

// Subprotocol is the Action Cable JSON protocol.
const Subprotocol = "actioncable-v1-json"

// DefaultChannel is the channel the developer's stream lives on.
const DefaultChannel = "DeveloperChannel"

// Timings. A variable, not a constant, only so the tests can shorten them.
const (
	DefaultPingTimeout = 10 * time.Second
	BackoffFloor       = time.Second
	BackoffCeiling     = 30 * time.Second
	handshakeTimeout   = 15 * time.Second
	// A frame carries one event body; the library's default of 32 KiB would
	// close the socket on a large one.
	readLimit = 4 << 20
)

var (
	// ErrSessionEnded means the credential was refused and nothing could renew
	// it: the person must log in again.
	ErrSessionEnded = errors.New("cable: the session ended")
	// ErrSubscriptionRejected means the server answered reject_subscription.
	ErrSubscriptionRejected = errors.New("cable: the server rejected the subscription")
	// ErrKeyRevoked means an API key was refused after the server closed the
	// socket remotely: the key was revoked. errors.Is(ErrKeyRevoked,
	// ErrSessionEnded) is true, so a caller that only knows the second still
	// ends the run, and a caller that knows the first can name the next step.
	ErrKeyRevoked error = keyRevoked{}
)

type keyRevoked struct{}

func (keyRevoked) Error() string        { return "cable: the API key was revoked" }
func (keyRevoked) Is(target error) bool { return target == ErrSessionEnded }

// Options configure a Client.
type Options struct {
	URL     string          // wss://cable.listo.delivery/cable
	Tokens  api.TokenSource // the bearer; Rejected is asked once on `unauthorized`
	Channel string          // default DefaultChannel

	UserAgent  string       // default version.UserAgent()
	HTTPClient *http.Client // default http.DefaultClient (tests carry a TLS client)

	PingTimeout time.Duration                   // default DefaultPingTimeout
	Backoff     func(attempt int) time.Duration // default BackoffFor
	// RateLimitWait is how long a 429 on the handshake waits; default BackoffCeiling.
	RateLimitWait time.Duration
	// Probe checks that the credential still works, with one authenticated read
	// (GET /v1/ping through an api.Client that shares Tokens, so a 401 already
	// asked the TokenSource to refresh). It is asked when the server closes the
	// socket with reason "remote" and reconnect false. Nil means: ask
	// Tokens.Rejected instead.
	Probe func(ctx context.Context) error
}

// NoticeKind says what a Notice is about.
type NoticeKind int

const (
	// NoticeReconnecting: the socket was lost; the client will try again after Wait.
	NoticeReconnecting NoticeKind = iota + 1
	// NoticeRateLimited: the handshake got a 429; the client waits Wait (the
	// ceiling of the backoff). Sent ONCE per run of 429s.
	NoticeRateLimited
	// NoticeClosedByServer: the server closed the socket and asked the client not
	// to reconnect (Reason); Run returns nil.
	NoticeClosedByServer
)

// Notice is something a person should be told about the connection.
type Notice struct {
	Kind   NoticeKind
	Wait   time.Duration
	Err    error  // why the connection was lost, for NoticeReconnecting
	Status int    // the handshake's HTTP status when it was refused
	Reason string // the server's disconnect reason
}

// Callbacks are what the client tells its user. Subscribed and Message run on
// the READ LOOP: they must return quickly (hand work to a goroutine or a queue),
// or the ping deadline is missed and the socket looks dead.
type Callbacks struct {
	// Subscribed is called for every confirm_subscription. first is true for the
	// first one of this Run.
	Subscribed func(first bool)
	// Message is called with the payload of every data frame.
	Message func(payload json.RawMessage)
	// Notice is called for things worth printing. May be nil.
	Notice func(Notice)
}

// Client is one logical connection that reconnects by itself.
type Client struct{ o Options }

// New builds a Client.
func New(o Options) *Client {
	if o.Channel == "" {
		o.Channel = DefaultChannel
	}
	if o.UserAgent == "" {
		o.UserAgent = version.UserAgent()
	}
	if o.PingTimeout <= 0 {
		o.PingTimeout = DefaultPingTimeout
	}
	if o.Backoff == nil {
		o.Backoff = BackoffFor
	}
	if o.RateLimitWait <= 0 {
		o.RateLimitWait = BackoffCeiling
	}
	return &Client{o: o}
}

// BackoffFor is the reconnect wait after `attempt` consecutive failures
// (0 is the first): 1 second doubling to 30, with jitter so a fleet of
// terminals does not reconnect in step (the wait is between half and all of it).
func BackoffFor(attempt int) time.Duration {
	d := BackoffCeiling
	if attempt < 5 { // 1, 2, 4, 8, 16, then the ceiling
		d = BackoffFloor << attempt
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(d-half)+1))
}

type outcomeKind int

const (
	outCanceled outcomeKind = iota
	outTransient
	outRateLimited
	outUnauthorized
	outRejected
	outClosedByServer
	outRemote // disconnect reason "remote", reconnect false: revoked, or not?
)

type outcome struct {
	kind      outcomeKind
	err       error
	status    int
	reason    string
	confirmed bool
}

// Run connects, subscribes and reads until the context ends, the credential is
// refused for good, or the server closes the socket and asks not to be
// reconnected. It reconnects by itself otherwise.
//
// It returns ctx.Err() when the context ends, nil when the server closed the
// socket without asking for a reconnect (a Notice says why), ErrSessionEnded
// when the credential was refused and could not be renewed (ErrKeyRevoked when
// a remote disconnect found a key revoked), and ErrSubscriptionRejected. An error from Tokens (a refresh that failed, "Session
// ended. Run listo login.") is returned as it came.
func (c *Client) Run(ctx context.Context, cb Callbacks) error {
	notice := func(n Notice) {
		if cb.Notice != nil {
			cb.Notice(n)
		}
	}
	first := true
	failures := 0
	refused := 0
	rateNotified := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := c.o.Tokens.Token(ctx)
		if err != nil {
			return err
		}
		out := c.session(ctx, token, cb, &first)
		if out.confirmed {
			failures, refused, rateNotified = 0, 0, false
		}
		switch out.kind {
		case outCanceled:
			return ctx.Err()

		case outUnauthorized:
			// Ask the token source ONCE: a grant refreshes and the next dial
			// carries the new bearer. A second refusal (without a confirmed
			// subscription in between) or a source that cannot renew ends it.
			refused++
			if refused > 1 {
				return ErrSessionEnded
			}
			retry, err := c.o.Tokens.Rejected(ctx, token)
			if err != nil {
				return err
			}
			if !retry {
				return ErrSessionEnded
			}
			continue

		case outRejected:
			return ErrSubscriptionRejected

		case outRemote:
			if err := c.checkCredential(ctx, token); err != nil {
				return err
			}
			// It still works: the server shut this connection for another
			// reason. Reconnect as from a dropped socket.
			wait := c.o.Backoff(failures)
			failures++
			notice(Notice{Kind: NoticeReconnecting, Wait: wait, Err: out.err})
			if err := sleep(ctx, wait); err != nil {
				return err
			}

		case outClosedByServer:
			notice(Notice{Kind: NoticeClosedByServer, Reason: out.reason})
			return nil

		case outRateLimited:
			wait := c.o.RateLimitWait
			if !rateNotified {
				rateNotified = true
				notice(Notice{Kind: NoticeRateLimited, Wait: wait, Status: out.status})
			}
			if err := sleep(ctx, wait); err != nil {
				return err
			}

		case outTransient:
			wait := c.o.Backoff(failures)
			failures++
			notice(Notice{Kind: NoticeReconnecting, Wait: wait, Err: out.err, Status: out.status})
			if err := sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
}

// checkCredential answers a remote disconnect: nil when the credential still
// works (reconnect), else the error that ends the run. A failure to find out
// (no answer, a 5xx, a 429) is nil too: the reconnect then learns the truth from
// the server's own "unauthorized" frame, which has its own path.
func (c *Client) checkCredential(ctx context.Context, token string) error {
	revoked := ErrSessionEnded
	if _, isKey := api.KeyMode(token); isKey {
		revoked = ErrKeyRevoked
	}
	if c.o.Probe == nil {
		retry, err := c.o.Tokens.Rejected(ctx, token)
		if err != nil {
			return err
		}
		if !retry {
			return revoked
		}
		return nil
	}
	err := c.o.Probe(ctx)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		if apiErr.Status == http.StatusUnauthorized {
			return revoked
		}
		return nil
	}
	var transport *api.TransportError
	if errors.As(err, &transport) {
		return nil
	}
	// Anything else came out of the TokenSource (a refresh that failed, "Session
	// ended. Run listo login."): returned as it came, like Run does.
	return err
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// frame is every field of an Action Cable frame the client reads.
type frame struct {
	Type       string          `json:"type"`
	Identifier string          `json:"identifier"`
	Message    json.RawMessage `json:"message"`
	Reason     string          `json:"reason"`
	Reconnect  *bool           `json:"reconnect"`
}

func (c *Client) identifier() string {
	b, _ := json.Marshal(map[string]string{"channel": c.o.Channel})
	return string(b)
}

// session is ONE connection, from dial to loss.
func (c *Client) session(ctx context.Context, token string, cb Callbacks, first *bool) (out outcome) {
	dctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	// No Origin header: a program sends none, and a bearer handshake that carries
	// one is refused with 404 (the package comment). Only these two headers go.
	conn, resp, err := websocket.Dial(dctx, c.o.URL, &websocket.DialOptions{
		HTTPClient:   c.o.HTTPClient,
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + token}, "User-Agent": {c.o.UserAgent}},
		Subprotocols: []string{Subprotocol},
	})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return outcome{kind: outCanceled}
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		switch status {
		case http.StatusTooManyRequests:
			return outcome{kind: outRateLimited, status: status, err: err}
		case http.StatusUnauthorized, http.StatusForbidden:
			return outcome{kind: outUnauthorized, status: status, err: err}
		}
		return outcome{kind: outTransient, status: status, err: err}
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)

	id := c.identifier()
	for {
		rctx, rcancel := context.WithTimeout(ctx, c.o.PingTimeout)
		_, data, err := conn.Read(rctx)
		timedOut := rctx.Err() != nil && ctx.Err() == nil
		rcancel()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				out.kind = outCanceled
			case timedOut:
				out.kind = outTransient
				out.err = fmt.Errorf("no ping from the server in %s", c.o.PingTimeout)
			default:
				out.kind = outTransient
				out.err = err
			}
			return out
		}

		var f frame
		if json.Unmarshal(data, &f) != nil {
			continue // a frame we cannot read is not a reason to drop the socket
		}
		switch f.Type {
		case "welcome":
			sub, _ := json.Marshal(map[string]string{"command": "subscribe", "identifier": id})
			if err := conn.Write(ctx, websocket.MessageText, sub); err != nil {
				out.kind, out.err = outTransient, err
				if ctx.Err() != nil {
					out.kind = outCanceled
				}
				return out
			}
		case "ping":
			// Reading it reset the deadline; nothing else to do.
		case "confirm_subscription":
			out.confirmed = true
			isFirst := *first
			*first = false
			if cb.Subscribed != nil {
				cb.Subscribed(isFirst)
			}
		case "reject_subscription":
			out.kind = outRejected
			return out
		case "disconnect":
			switch {
			case f.Reason == "unauthorized":
				out.kind, out.reason = outUnauthorized, f.Reason
			case f.Reason == "remote" && f.Reconnect != nil && !*f.Reconnect:
				out.kind, out.reason = outRemote, f.Reason
				out.err = fmt.Errorf("the server closed the connection (%s)", f.Reason)
			case f.Reconnect != nil && !*f.Reconnect:
				out.kind, out.reason = outClosedByServer, f.Reason
			default:
				out.kind, out.reason = outTransient, f.Reason
				out.err = fmt.Errorf("the server closed the connection (%s)", f.Reason)
			}
			return out
		case "":
			if len(f.Message) > 0 && (f.Identifier == "" || f.Identifier == id) && cb.Message != nil {
				cb.Message(f.Message)
			}
		}
	}
}
