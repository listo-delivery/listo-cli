package listen_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/cable"
	"github.com/listo-delivery/listo-cli/internal/cable/cabletest"
	"github.com/listo-delivery/listo-cli/internal/listen"
	"github.com/listo-delivery/listo-cli/internal/signature"
)

const secret = "whsec_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// A body with the spacing, key order and unicode escapes of Rails' to_json: any
// parse-and-reencode would change these bytes and break the signature.
func envelope(id, typ, created string) string {
	return `{"id":"` + id + `","type":"` + typ + `","created_at":"` + created + `","data":{"note":"café <","n":1.0}}`
}

type received struct {
	body   []byte
	header http.Header
}

// local is the developer's own server.
type local struct {
	*httptest.Server
	mu   sync.Mutex
	got  []received
	code int
}

func newLocal(t *testing.T, code int) *local {
	l := &local{code: code}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.got = append(l.got, received{body: b, header: r.Header.Clone()})
		l.mu.Unlock()
		w.WriteHeader(l.code)
	}))
	t.Cleanup(l.Close)
	return l
}

func (l *local) received() []received {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]received(nil), l.got...)
}

func (l *local) bodies() []string {
	var out []string
	for _, r := range l.received() {
		out = append(out, string(r.body))
	}
	return out
}

// fakeAPI serves GET /v1/events from a fixed newest-first list.
type fakeAPI struct {
	*httptest.Server
	mu    sync.Mutex
	items []string // raw JSON, newest first
	query []string
}

func newFakeAPI(t *testing.T, items ...string) *fakeAPI {
	f := &fakeAPI{items: items}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.query = append(f.query, r.URL.RawQuery)
		items := append([]string(nil), f.items...)
		f.mu.Unlock()
		if r.URL.Path != "/v1/events" {
			http.NotFound(w, r)
			return
		}
		out := `{"data":[`
		for i, it := range items {
			if i > 0 {
				out += ","
			}
			out += it
		}
		out += `],"has_more":false,"cursor":null}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(f.Close)
	return f
}

func frameEvent(c *cabletest.Conn, id, typ, created string) string {
	body := envelope(id, typ, created)
	_ = c.Event(id, typ, body)
	return body
}

func within(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func cableOpts(url string) cable.Options {
	return cable.Options{
		URL: url, Tokens: api.StaticKey("lst_test_x"), UserAgent: "listo-cli/test",
		PingTimeout: 2 * time.Second,
		Backoff:     func(int) time.Duration { return 10 * time.Millisecond },
	}
}

type run struct {
	results []listen.Result
	mu      sync.Mutex
	ready   int
	missed  []int
	done    chan error
	cancel  context.CancelFunc
}

func (r *run) all() []listen.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]listen.Result(nil), r.results...)
}

func start(t *testing.T, srv *cabletest.Server, loc *local, apiURL string, types map[string]bool) *run {
	r := &run{done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	s := &listen.Session{
		Client:    api.New(api.Options{BaseURL: apiURL, Tokens: api.StaticKey("lst_test_x")}),
		Cable:     cableOpts(srv.WSURL()),
		Forwarder: &listen.Forwarder{URL: loc.URL, Secret: secret},
		Types:     types,
		Now:       func() time.Time { return time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC) },
		Ready:     func() { r.mu.Lock(); r.ready++; r.mu.Unlock() },
		Result:    func(res listen.Result) { r.mu.Lock(); r.results = append(r.results, res); r.mu.Unlock() },
		CatchUp:   func(n int, _ error) { r.mu.Lock(); r.missed = append(r.missed, n); r.mu.Unlock() },
	}
	go func() { r.done <- s.Run(ctx) }()
	return r
}

func TestForwardedBodyIsTheFramesBodyByteForByteAndVerifies(t *testing.T) {
	sentCh := make(chan string, 1)
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		sentCh <- frameEvent(c, "evt_1", "delivery.status_changed", "2026-11-09T18:00:00Z")
		c.Hold(2 * time.Second)
	})
	loc := newLocal(t, 200)
	r := start(t, srv, loc, newFakeAPI(t).URL, nil)
	within(t, "the forward", func() bool { return len(loc.received()) == 1 })
	r.cancel()

	sent := <-sentCh
	got := loc.received()[0]
	if string(got.body) != sent {
		t.Fatalf("body changed on the way:\n sent %s\n got  %s", sent, got.body)
	}
	if !signature.Verify(secret, got.body, got.header.Get(signature.Header), time.Now(), 0) {
		t.Errorf("X-Listo-Signature %q does not verify against the printed secret", got.header.Get(signature.Header))
	}
	if signature.Verify("whsec_other", got.body, got.header.Get(signature.Header), time.Now(), 0) {
		t.Error("the signature verifies under another secret")
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if ua := got.header.Get("User-Agent"); !strings.HasPrefix(ua, "listo-cli/") {
		t.Errorf("User-Agent = %q", ua)
	}
	res := r.all()
	if len(res) != 1 || !res[0].OK() || res[0].Status != 200 || res[0].Type != "delivery.status_changed" || res[0].ID != "evt_1" {
		t.Errorf("result = %+v", res)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready != 1 {
		t.Errorf("Ready called %d times", r.ready)
	}
}

func TestANon2xxIsReportedAndNotRetried(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		frameEvent(c, "evt_1", "delivery.updated", "2026-11-09T18:00:00Z")
		c.Hold(time.Second)
	})
	loc := newLocal(t, 500)
	r := start(t, srv, loc, newFakeAPI(t).URL, nil)
	within(t, "the result", func() bool { return len(r.all()) == 1 })
	time.Sleep(150 * time.Millisecond)
	r.cancel()
	res := r.all()[0]
	if res.OK() || res.Status != 500 || res.Err != nil {
		t.Errorf("result = %+v", res)
	}
	if n := len(loc.received()); n != 1 {
		t.Errorf("the local server was called %d times, want 1 (no retry)", n)
	}
}

func TestAnUnreachableURLIsReportedAsSuch(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		frameEvent(c, "evt_1", "delivery.updated", "2026-11-09T18:00:00Z")
		c.Hold(time.Second)
	})
	loc := newLocal(t, 200)
	loc.Close() // nobody is listening there any more
	r := start(t, srv, loc, newFakeAPI(t).URL, nil)
	within(t, "the result", func() bool { return len(r.all()) == 1 })
	r.cancel()
	res := r.all()[0]
	if res.Err == nil || res.Status != 0 || res.OK() {
		t.Errorf("result = %+v", res)
	}
}

func TestEventsFlagFiltersByType(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		frameEvent(c, "evt_1", "delivery.courier_location", "2026-11-09T18:00:00Z")
		frameEvent(c, "evt_2", "delivery.status_changed", "2026-11-09T18:00:01Z")
		frameEvent(c, "evt_3", "delivery.rated", "2026-11-09T18:00:02Z")
		c.Hold(time.Second)
	})
	loc := newLocal(t, 200)
	r := start(t, srv, loc, newFakeAPI(t).URL, map[string]bool{"delivery.status_changed": true, "delivery.rated": true})
	within(t, "two forwards", func() bool { return len(r.all()) == 2 })
	time.Sleep(150 * time.Millisecond)
	r.cancel()
	res := r.all()
	if len(res) != 2 || res[0].ID != "evt_2" || res[1].ID != "evt_3" {
		t.Errorf("results = %+v", res)
	}
}

func TestOtherFramesAreIgnored(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		_ = c.Send(map[string]any{"type": "request_log", "log": map[string]any{"id": "req_1"}})
		_ = c.Send(map[string]any{"type": "something_new", "id": "x"})
		frameEvent(c, "evt_1", "delivery.updated", "2026-11-09T18:00:00Z")
		c.Hold(time.Second)
	})
	loc := newLocal(t, 200)
	r := start(t, srv, loc, newFakeAPI(t).URL, nil)
	within(t, "the event", func() bool { return len(r.all()) == 1 })
	time.Sleep(100 * time.Millisecond)
	r.cancel()
	if len(r.all()) != 1 {
		t.Errorf("results = %+v", r.all())
	}
}

func TestReconnectCatchesUpOnlyWhatWasMissedInOrderWithoutDuplicates(t *testing.T) {
	e1 := envelope("evt_1", "delivery.status_changed", "2026-11-09T18:00:00Z")
	e2 := envelope("evt_2", "delivery.status_changed", "2026-11-09T18:00:05Z")
	e3 := envelope("evt_3", "delivery.updated", "2026-11-09T18:00:09Z")
	// The list: newest first, and it holds events already seen (evt_1) and one
	// the new socket also delivers (evt_3).
	fake := newFakeAPI(t, e3, e2, e1)

	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index == 0 {
			_ = c.Event("evt_1", "delivery.status_changed", e1)
			time.Sleep(80 * time.Millisecond)
			c.Drop()
			return
		}
		_ = c.Event("evt_3", "delivery.updated", e3)
		c.Hold(2 * time.Second)
	})
	loc := newLocal(t, 200)
	r := start(t, srv, loc, fake.URL, nil)
	within(t, "three forwards", func() bool { return len(loc.received()) >= 3 })
	time.Sleep(200 * time.Millisecond) // a duplicate would arrive here
	r.cancel()

	got := loc.bodies()
	if len(got) != 3 || got[0] != e1 || got[1] != e2 || got[2] != e3 {
		t.Fatalf("forwarded %d bodies, want evt_1, evt_2, evt_3 once each in order: %q", len(got), got)
	}
	// Every forward is signed, including the ones the list supplied.
	for i, rc := range loc.received() {
		if !signature.Verify(secret, rc.body, rc.header.Get(signature.Header), time.Now(), 0) {
			t.Errorf("forward %d does not verify", i)
		}
	}
	// The list was read from the last seen event's created_at minus 60 seconds.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.query) != 1 {
		t.Fatalf("list reads = %v", fake.query)
	}
	if want := "created_after=2026-11-09T17%3A59%3A00Z"; fake.query[0] != want {
		t.Errorf("query = %q, want %q", fake.query[0], want)
	}
}
