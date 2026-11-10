package cable_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/cable"
	"github.com/listo-delivery/listo-cli/internal/cable/cabletest"
)

// recorder collects the callbacks of a Run.
type recorder struct {
	mu       sync.Mutex
	messages []string
	firsts   []bool
	notices  []cable.Notice
}

func (r *recorder) callbacks() cable.Callbacks {
	return cable.Callbacks{
		Subscribed: func(first bool) { r.mu.Lock(); r.firsts = append(r.firsts, first); r.mu.Unlock() },
		Message:    func(p json.RawMessage) { r.mu.Lock(); r.messages = append(r.messages, string(p)); r.mu.Unlock() },
		Notice:     func(n cable.Notice) { r.mu.Lock(); r.notices = append(r.notices, n); r.mu.Unlock() },
	}
}

func (r *recorder) msgs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.messages...)
}

// subscriptions is the `first` flag of every Subscribed callback so far.
func (r *recorder) subscriptions() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.firsts...)
}

func (r *recorder) noticeKinds() []cable.NoticeKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []cable.NoticeKind
	for _, n := range r.notices {
		out = append(out, n.Kind)
	}
	return out
}

func fast(url string, tok api.TokenSource) cable.Options {
	return cable.Options{
		URL: url, Tokens: tok, UserAgent: "listo-cli/test",
		PingTimeout:   2 * time.Second,
		Backoff:       func(int) time.Duration { return 10 * time.Millisecond },
		RateLimitWait: 80 * time.Millisecond,
	}
}

func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func runAsync(ctx context.Context, c *cable.Client, cb cable.Callbacks) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, cb) }()
	return done
}

func waitErr(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func TestHandshakeSendsBearerAndNoOriginAndSubprotocol(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		_ = c.Event("evt_1", "delivery.updated", `{"id":"evt_1"}`)
		c.Hold(time.Second)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{}
	done := runAsync(ctx, cable.New(fast(srv.WSURL(), api.StaticKey("lst_test_secret"))), rec.callbacks())
	within(t, 3*time.Second, "a message", func() bool { return len(rec.msgs()) == 1 })
	cancel()
	if err := waitErr(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}

	h := srv.Headers()[0]
	if got := h.Get("Authorization"); got != "Bearer lst_test_secret" {
		t.Errorf("Authorization = %q", got)
	}
	if _, has := h["Origin"]; has {
		t.Errorf("an Origin header was sent: %q (a bearer handshake with one is refused with 404)", h.Get("Origin"))
	}
	if got := h.Get("Sec-WebSocket-Protocol"); got != "actioncable-v1-json" {
		t.Errorf("subprotocol = %q", got)
	}
	if got := h.Get("User-Agent"); got != "listo-cli/test" {
		t.Errorf("User-Agent = %q", got)
	}
	if rec.firsts[0] != true {
		t.Errorf("first confirm should be first=true")
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(rec.msgs()[0]), &m)
	if m["type"] != "event" || m["id"] != "evt_1" {
		t.Errorf("message = %v", m)
	}
}

func TestSubscribesWithTheDeveloperChannelIdentifier(t *testing.T) {
	got := make(chan string, 1)
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Welcome()
		id, _ := c.ExpectSubscribe()
		got <- id
		c.Hold(time.Second)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(ctx, cable.New(fast(srv.WSURL(), api.StaticKey("k"))), cable.Callbacks{})
	select {
	case id := <-got:
		if id != cabletest.Identifier {
			t.Errorf("identifier = %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no subscribe")
	}
	cancel()
	waitErr(t, done)
}

func TestPingsKeepTheSocketAliveAndSilenceKillsIt(t *testing.T) {
	var conns atomic.Int32
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		conns.Add(1)
		_ = c.Handshake()
		if c.Index == 0 {
			// Silent: no pings at all. The client must call it dead.
			c.Hold(5 * time.Second)
			return
		}
		for i := 0; i < 5; i++ {
			_ = c.Ping()
			time.Sleep(60 * time.Millisecond)
		}
		c.Hold(time.Second)
	})
	o := fast(srv.WSURL(), api.StaticKey("k"))
	o.PingTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{}
	done := runAsync(ctx, cable.New(o), rec.callbacks())
	// Wait for the EVENT the assertions are about: the second connection's
	// confirm_subscription reaching the client. The server's connection count
	// moves when the script STARTS, before its handshake, so waiting on it let
	// cancel() win the race against Subscribed(false).
	within(t, 3*time.Second, "a second confirmed subscription", func() bool { return len(rec.subscriptions()) >= 2 })
	cancel()
	waitErr(t, done)
	if conns.Load() < 2 {
		t.Errorf("connections = %d, want a reconnect", conns.Load())
	}
	rec.mu.Lock()
	notices := append([]cable.Notice(nil), rec.notices...)
	rec.mu.Unlock()
	if len(notices) == 0 || notices[0].Kind != cable.NoticeReconnecting {
		t.Fatalf("notices = %v", notices)
	}
	if !strings.Contains(notices[0].Err.Error(), "no ping") {
		t.Errorf("err = %v", notices[0].Err)
	}
	if got := rec.subscriptions(); len(got) < 2 || !got[0] || got[1] {
		t.Errorf("firsts = %v, want [true false ...]", got)
	}
}

func TestADroppedConnectionReconnects(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index == 0 {
			_ = c.Event("evt_a", "delivery.updated", `{}`)
			time.Sleep(30 * time.Millisecond)
			c.Drop()
			return
		}
		_ = c.Event("evt_b", "delivery.updated", `{}`)
		c.Hold(time.Second)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{}
	done := runAsync(ctx, cable.New(fast(srv.WSURL(), api.StaticKey("k"))), rec.callbacks())
	within(t, 3*time.Second, "both messages", func() bool { return len(rec.msgs()) == 2 })
	cancel()
	waitErr(t, done)
	if srv.Attempts() != 2 {
		t.Errorf("attempts = %d", srv.Attempts())
	}
	if k := rec.noticeKinds(); len(k) != 1 || k[0] != cable.NoticeReconnecting {
		t.Errorf("notices = %v", k)
	}
}

// grantTokens is a TokenSource that behaves like a refreshing grant.
type grantTokens struct {
	mu       sync.Mutex
	current  string
	rejected []string
	renew    func(old string) (string, error)
}

func (g *grantTokens) Token(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.current, nil
}

func (g *grantTokens) Rejected(_ context.Context, token string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rejected = append(g.rejected, token)
	if g.renew == nil {
		return false, nil
	}
	n, err := g.renew(token)
	if err != nil {
		return false, err
	}
	g.current = n
	return true, nil
}

func TestUnauthorizedAsksTheTokenSourceOnceAndRetriesWithTheNewToken(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		if strings.HasSuffix(c.Req.Header.Get("Authorization"), "old") {
			_ = c.Disconnect("unauthorized", false)
			return
		}
		_ = c.Handshake()
		c.Hold(time.Second)
	})
	g := &grantTokens{current: "old", renew: func(string) (string, error) { return "new", nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{}
	done := runAsync(ctx, cable.New(fast(srv.WSURL(), g)), rec.callbacks())
	within(t, 3*time.Second, "a confirmed subscription", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.firsts) == 1
	})
	cancel()
	waitErr(t, done)
	if len(g.rejected) != 1 || g.rejected[0] != "old" {
		t.Errorf("Rejected calls = %v", g.rejected)
	}
	if got := srv.Headers()[1].Get("Authorization"); got != "Bearer new" {
		t.Errorf("second handshake carried %q", got)
	}
}

func TestASecondUnauthorizedEndsTheSession(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) { _ = c.Disconnect("unauthorized", false) })
	g := &grantTokens{current: "t1", renew: func(string) (string, error) { return "t2", nil }}
	done := runAsync(context.Background(), cable.New(fast(srv.WSURL(), g)), cable.Callbacks{})
	if err := waitErr(t, done); !errors.Is(err, cable.ErrSessionEnded) {
		t.Fatalf("err = %v", err)
	}
	if len(g.rejected) != 1 {
		t.Errorf("Rejected called %d times, want once", len(g.rejected))
	}
	if srv.Attempts() != 2 {
		t.Errorf("attempts = %d", srv.Attempts())
	}
}

func TestAKeyThatIsRefusedEndsAtOnce(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) { _ = c.Disconnect("unauthorized", false) })
	done := runAsync(context.Background(), cable.New(fast(srv.WSURL(), api.StaticKey("lst_test_bad"))), cable.Callbacks{})
	if err := waitErr(t, done); !errors.Is(err, cable.ErrSessionEnded) {
		t.Fatalf("err = %v", err)
	}
	if srv.Attempts() != 1 {
		t.Errorf("attempts = %d", srv.Attempts())
	}
}

func TestAFailedRefreshReturnsItsOwnError(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) { _ = c.Disconnect("unauthorized", false) })
	boom := errors.New("Session ended. Run listo login.")
	g := &grantTokens{current: "t1", renew: func(string) (string, error) { return "", boom }}
	done := runAsync(context.Background(), cable.New(fast(srv.WSURL(), g)), cable.Callbacks{})
	if err := waitErr(t, done); err != boom {
		t.Fatalf("err = %v", err)
	}
}

func TestADisconnectThatAsksNotToReconnectIsACleanEndWithinAboutASecond(t *testing.T) {
	// Any reason but "remote" with reconnect false: the server said not to come back.
	revoke := make(chan struct{})
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		<-revoke
		_ = c.Disconnect("server_closing", false)
		c.Hold(time.Second)
	})
	rec := &recorder{}
	done := runAsync(context.Background(), cable.New(fast(srv.WSURL(), api.StaticKey("k"))), rec.callbacks())
	within(t, 3*time.Second, "subscribed", func() bool { rec.mu.Lock(); defer rec.mu.Unlock(); return len(rec.firsts) == 1 })
	start := time.Now()
	close(revoke)
	if err := waitErr(t, done); err != nil {
		t.Fatalf("a clean end should return nil, got %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %s to end", took)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.notices) != 1 || rec.notices[0].Kind != cable.NoticeClosedByServer || rec.notices[0].Reason != "server_closing" {
		t.Errorf("notices = %+v", rec.notices)
	}
	if srv.Attempts() != 1 {
		t.Errorf("reconnected after reconnect:false (%d attempts)", srv.Attempts())
	}
}

// remoteServer closes the first connection the way PartnerApiKey#revoke! does
// (reason "remote", reconnect false) and holds any later one open.
func remoteServer(t *testing.T) *cabletest.Server {
	t.Helper()
	return cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index == 0 {
			_ = c.Disconnect("remote", false)
			return
		}
		c.Hold(time.Second)
	})
}

func refused() error { return &api.Error{Status: 401, Code: "invalid_api_key"} }

func TestARemoteDisconnectOfARevokedKeyEndsTheRunAsARevokedKey(t *testing.T) {
	srv := remoteServer(t)
	o := fast(srv.WSURL(), api.StaticKey("lst_test_abc"))
	o.Probe = func(context.Context) error { return refused() }
	start := time.Now()
	err := waitErr(t, runAsync(context.Background(), cable.New(o), cable.Callbacks{}))
	if !errors.Is(err, cable.ErrKeyRevoked) || !errors.Is(err, cable.ErrSessionEnded) {
		t.Fatalf("err = %v, want ErrKeyRevoked (which is also ErrSessionEnded)", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %s to end", took)
	}
	if srv.Attempts() != 1 {
		t.Errorf("reconnected a revoked key (%d attempts)", srv.Attempts())
	}
}

func TestARemoteDisconnectOfARevokedGrantEndsTheRunAsASessionEnded(t *testing.T) {
	srv := remoteServer(t)
	g := &grantTokens{current: "lso_at_grant"}
	o := fast(srv.WSURL(), g)
	o.Probe = func(context.Context) error { return refused() }
	err := waitErr(t, runAsync(context.Background(), cable.New(o), cable.Callbacks{}))
	if !errors.Is(err, cable.ErrSessionEnded) || errors.Is(err, cable.ErrKeyRevoked) {
		t.Fatalf("err = %v, want ErrSessionEnded and not ErrKeyRevoked", err)
	}
}

func TestARemoteDisconnectReturnsTheTokenSourcesOwnErrorWhenTheRefreshFails(t *testing.T) {
	srv := remoteServer(t)
	boom := errors.New("Session ended. Run listo login.")
	o := fast(srv.WSURL(), &grantTokens{current: "t1"})
	o.Probe = func(context.Context) error { return boom } // what api.Client returns when Rejected fails
	if err := waitErr(t, runAsync(context.Background(), cable.New(o), cable.Callbacks{})); err != boom {
		t.Fatalf("err = %v", err)
	}
}

func TestARemoteDisconnectWithoutAProbeAsksTheTokenSource(t *testing.T) {
	// A grant that can refresh is alive: reconnect. A key cannot: revoked.
	srv := remoteServer(t)
	g := &grantTokens{current: "t1", renew: func(string) (string, error) { return "t2", nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{}
	done := runAsync(ctx, cable.New(fast(srv.WSURL(), g)), rec.callbacks())
	within(t, 3*time.Second, "the second subscription", func() bool { rec.mu.Lock(); defer rec.mu.Unlock(); return len(rec.firsts) == 2 })
	cancel()
	waitErr(t, done)
	if len(g.rejected) != 1 {
		t.Errorf("Rejected asked %d times, want 1", len(g.rejected))
	}

	srv = remoteServer(t)
	err := waitErr(t, runAsync(context.Background(), cable.New(fast(srv.WSURL(), api.StaticKey("lst_live_abc"))), cable.Callbacks{}))
	if !errors.Is(err, cable.ErrKeyRevoked) {
		t.Fatalf("err = %v", err)
	}
}

func TestARemoteDisconnectOfACredentialThatStillWorksReconnects(t *testing.T) {
	srv := remoteServer(t)
	o := fast(srv.WSURL(), api.StaticKey("lst_test_abc"))
	var probes atomic.Int32
	o.Probe = func(context.Context) error { probes.Add(1); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{}
	done := runAsync(ctx, cable.New(o), rec.callbacks())
	within(t, 3*time.Second, "the second subscription", func() bool { rec.mu.Lock(); defer rec.mu.Unlock(); return len(rec.firsts) == 2 })
	cancel()
	waitErr(t, done)
	if probes.Load() != 1 || srv.Attempts() != 2 {
		t.Errorf("probes = %d, attempts = %d, want 1 and 2", probes.Load(), srv.Attempts())
	}
	if k := rec.noticeKinds(); len(k) != 1 || k[0] != cable.NoticeReconnecting {
		t.Errorf("notices = %v, want one reconnecting (not a closed-by-server)", k)
	}
}

func TestARemoteDisconnectWhenTheCheckCannotAnswerReconnects(t *testing.T) {
	// The probe failed for a reason that says nothing about the credential (no
	// network, a 503): reconnect, and the server's own "unauthorized" frame
	// decides if it is dead.
	for name, perr := range map[string]error{
		"transport": &api.TransportError{Err: errors.New("connection refused")},
		"server":    &api.Error{Status: 503, Code: "unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := remoteServer(t)
			o := fast(srv.WSURL(), api.StaticKey("lst_test_abc"))
			o.Probe = func(context.Context) error { return perr }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := &recorder{}
			done := runAsync(ctx, cable.New(o), rec.callbacks())
			within(t, 3*time.Second, "the second subscription", func() bool { rec.mu.Lock(); defer rec.mu.Unlock(); return len(rec.firsts) == 2 })
			cancel()
			waitErr(t, done)
		})
	}
}

func TestADisconnectThatAllowsReconnectReconnects(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index == 0 {
			_ = c.Disconnect("server_restart", true)
			return
		}
		c.Hold(time.Second)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(ctx, cable.New(fast(srv.WSURL(), api.StaticKey("k"))), cable.Callbacks{})
	within(t, 3*time.Second, "a second handshake", func() bool { return srv.Attempts() == 2 })
	cancel()
	waitErr(t, done)
}

func TestARejectedSubscriptionIsAnError(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Welcome()
		_, _ = c.ExpectSubscribe()
		_ = c.SendRaw(`{"type":"reject_subscription","identifier":"x"}`)
		c.Hold(time.Second)
	})
	done := runAsync(context.Background(), cable.New(fast(srv.WSURL(), api.StaticKey("k"))), cable.Callbacks{})
	if err := waitErr(t, done); !errors.Is(err, cable.ErrSubscriptionRejected) {
		t.Fatalf("err = %v", err)
	}
}

func TestA429OnTheHandshakeWaitsAtTheCeilingAndSaysSoOnce(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		c.Hold(time.Second)
	})
	srv.Gate = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{}
	start := time.Now()
	done := runAsync(ctx, cable.New(fast(srv.WSURL(), api.StaticKey("k"))), rec.callbacks())
	within(t, 3*time.Second, "subscription after the 429s", func() bool { rec.mu.Lock(); defer rec.mu.Unlock(); return len(rec.firsts) == 1 })
	cancel()
	waitErr(t, done)
	if took := time.Since(start); took < 3*80*time.Millisecond {
		t.Errorf("three 429s took only %s; each should wait RateLimitWait (80ms)", took)
	}
	var rate int
	for _, k := range rec.noticeKinds() {
		if k == cable.NoticeRateLimited {
			rate++
		}
	}
	if rate != 1 {
		t.Errorf("rate-limit notices = %d, want exactly 1", rate)
	}
	if rec.notices[0].Wait != 80*time.Millisecond || rec.notices[0].Status != 429 {
		t.Errorf("notice = %+v", rec.notices[0])
	}
}

func TestA401OnTheHandshakeIsReadAsUnauthorized(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {})
	srv.Gate = func(n int, w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusUnauthorized)
		return true
	}
	done := runAsync(context.Background(), cable.New(fast(srv.WSURL(), api.StaticKey("k"))), cable.Callbacks{})
	if err := waitErr(t, done); !errors.Is(err, cable.ErrSessionEnded) {
		t.Fatalf("err = %v", err)
	}
}

func TestBackoffGrowsFromOneSecondToThirtyWithJitter(t *testing.T) {
	for attempt, want := range []time.Duration{1, 2, 4, 8, 16, 30, 30, 30} {
		ceil := want * time.Second
		for i := 0; i < 200; i++ {
			got := cable.BackoffFor(attempt)
			if got > ceil || got < ceil/2 {
				t.Fatalf("attempt %d: %s outside [%s, %s]", attempt, got, ceil/2, ceil)
			}
		}
	}
}
