package logs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/cable"
	"github.com/listo-delivery/listo-cli/internal/cable/cabletest"
	"github.com/listo-delivery/listo-cli/internal/logs"
)

func ptr[T any](v T) *T { return &v }

func row(id, endpoint string, status int, prefix *string) api.RequestLog {
	return api.RequestLog{ID: id, Method: "GET", Endpoint: endpoint, Status: status, APIKeyPrefix: prefix, CreatedAt: "2026-11-09T18:00:00Z"}
}

const keyToken = "lst_test_AbCdEfGh1234secretsecret"

func TestFilterStatus(t *testing.T) {
	ok, bad := row("a", "api/v1/ping#show", 200, nil), row("b", "api/v1/deliveries#create", 422, nil)
	redirect := row("c", "api/v1/ping#show", 399, nil)
	cases := []struct {
		status   string
		ok, bad  bool
		redirect bool
	}{
		{"", true, true, true},
		{"failed", false, true, false},
		{"succeeded", true, false, true},
	}
	for _, c := range cases {
		f := logs.Filter{Status: c.status}
		if f.Show(ok) != c.ok || f.Show(bad) != c.bad || f.Show(redirect) != c.redirect {
			t.Errorf("status %q: ok=%v bad=%v redirect=%v", c.status, f.Show(ok), f.Show(bad), f.Show(redirect))
		}
	}
	if !(logs.Filter{Status: "failed"}).Show(row("d", "x", 400, nil)) {
		t.Error("400 is failed")
	}
}

func TestFilterEndpoint(t *testing.T) {
	f := logs.Filter{Endpoint: "api/v1/deliveries#create"}
	if !f.Show(row("a", "api/v1/deliveries#create", 201, nil)) || f.Show(row("b", "api/v1/ping#show", 200, nil)) {
		t.Error("endpoint filter")
	}
}

func TestOwnCatchUpReadsAreHiddenForASignInByEndpointAndNullPrefix(t *testing.T) {
	f := logs.Filter{Token: "lso_at_somegrant"} // a CLI sign-in token: rows carry no key prefix
	for _, ep := range []string{"api/v1/events#index", "api/v1/request_logs#index"} {
		if f.Show(row("a", ep, 200, nil)) {
			t.Errorf("%s with no prefix should be hidden for a sign-in", ep)
		}
	}
	// Another integration's own read of the same list, made with a key, shows.
	if !f.Show(row("b", "api/v1/events#index", 200, ptr("lst_test_Abc1"))) {
		t.Error("a read made with a key is somebody else's, not ours")
	}
	// A different endpoint is never ours.
	if !f.Show(row("c", "api/v1/deliveries#index", 200, nil)) {
		t.Error("deliveries#index is not a catch-up read")
	}
	// --all shows them.
	f.All = true
	if !f.Show(row("d", "api/v1/events#index", 200, nil)) {
		t.Error("--all shows the CLI's own reads")
	}
}

func TestOwnCatchUpReadsAreHiddenForAKeyByEndpointAndMatchingPrefix(t *testing.T) {
	f := logs.Filter{Token: keyToken}
	mine := ptr(keyToken[:logs.KeyPrefixLength])
	if f.Show(row("a", "api/v1/request_logs#index", 200, mine)) {
		t.Error("a catch-up read with our key's prefix should be hidden")
	}
	if !f.Show(row("b", "api/v1/request_logs#index", 200, ptr("lst_test_Zzz9"))) {
		t.Error("another key's read of the log is shown")
	}
	if !f.Show(row("c", "api/v1/request_logs#index", 200, nil)) {
		t.Error("a null prefix is a sign-in's row, not this key's")
	}
	if !f.Show(row("d", "api/v1/ping#show", 200, mine)) {
		t.Error("our key's other calls show")
	}
}

type fakeAPI struct {
	*httptest.Server
	mu    sync.Mutex
	query []string
}

func newFakeAPI(t *testing.T, items ...string) *fakeAPI {
	f := &fakeAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.query = append(f.query, r.URL.RawQuery)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[` + strings.Join(items, ",") + `],"has_more":false,"cursor":null}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func logJSON(id, endpoint string, status int, at string) string {
	return `{"id":"` + id + `","method":"GET","path":"/v1/x","endpoint":"` + endpoint + `","status":` +
		itoa(status) + `,"error_code":null,"duration_ms":12,"request_id":"r-` + id + `","api_key_prefix":null,"test":true,"created_at":"` + at + `"}`
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestSessionPrintsLiveRowsFiltersAndCatchesUpAfterAReconnect(t *testing.T) {
	r1 := logJSON("req_1", "api/v1/deliveries#create", 201, "2026-11-09T18:00:00Z")
	r2 := logJSON("req_2", "api/v1/deliveries#create", 422, "2026-11-09T18:00:04Z")
	own := logJSON("req_3", "api/v1/request_logs#index", 200, "2026-11-09T18:00:05Z")
	r4 := logJSON("req_4", "api/v1/ping#show", 200, "2026-11-09T18:00:08Z")
	fake := newFakeAPI(t, r4, own, r2, r1) // newest first

	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index == 0 {
			_ = c.SendRaw(`{"identifier":"` + strings.ReplaceAll(cabletest.Identifier, `"`, `\"`) + `","message":{"type":"request_log","log":` + r1 + `}}`)
			time.Sleep(80 * time.Millisecond)
			c.Drop()
			return
		}
		c.Hold(2 * time.Second)
	})

	var mu sync.Mutex
	var rows []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &logs.Session{
		Client: api.New(api.Options{BaseURL: fake.URL, Tokens: api.StaticKey(keyToken)}),
		Cable: cable.Options{
			URL: srv.WSURL(), Tokens: api.StaticKey(keyToken), PingTimeout: 2 * time.Second,
			Backoff: func(int) time.Duration { return 10 * time.Millisecond },
		},
		Filter: logs.Filter{Token: "lso_at_grant"}, // sign-in: the list's own rows have no prefix
		Now:    func() time.Time { return time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC) },
		Row:    func(l api.RequestLog) { mu.Lock(); rows = append(rows, l.ID); mu.Unlock() },
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(rows)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	want := []string{"req_1", "req_2", "req_4"} // req_3 is the CLI's own catch-up read: hidden
	if strings.Join(rows, ",") != strings.Join(want, ",") {
		t.Errorf("rows = %v, want %v (in order, once each, own read hidden)", rows, want)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.query) != 1 || fake.query[0] != "created_after=2026-11-09T17%3A59%3A00Z" {
		t.Errorf("list reads = %v", fake.query)
	}
}

func TestCatchUpNeverPrintsARowFromTheSecondTheSessionStarted(t *testing.T) {
	// Started at 18:00:00.4; the list stamps a row created at 18:00:00.1 as
	// 18:00:00 and returns it for created_after=18:00:00.
	before := logJSON("req_before", "api/v1/deliveries#create", 201, "2026-11-09T18:00:00Z")
	missed := logJSON("req_missed", "api/v1/deliveries#create", 422, "2026-11-09T18:00:03Z")
	fake := newFakeAPI(t, missed, before)

	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index == 0 {
			time.Sleep(50 * time.Millisecond)
			c.Drop()
			return
		}
		c.Hold(2 * time.Second)
	})

	var mu sync.Mutex
	var rows []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &logs.Session{
		Client: api.New(api.Options{BaseURL: fake.URL, Tokens: api.StaticKey(keyToken)}),
		Cable: cable.Options{
			URL: srv.WSURL(), Tokens: api.StaticKey(keyToken), PingTimeout: 2 * time.Second,
			Backoff: func(int) time.Duration { return 10 * time.Millisecond },
		},
		Filter: logs.Filter{Token: "lso_at_grant"},
		Now:    func() time.Time { return time.Date(2026, 11, 9, 18, 0, 0, 400_000_000, time.UTC) },
		Row:    func(l api.RequestLog) { mu.Lock(); rows = append(rows, l.ID); mu.Unlock() },
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(rows)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(rows, ",") != "req_missed" {
		t.Errorf("rows = %v, want only [req_missed]", rows)
	}
}
