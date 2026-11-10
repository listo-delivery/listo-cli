package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/version"
)

func newTestClient(t *testing.T, h http.HandlerFunc, tokens TokenSource) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(Options{BaseURL: srv.URL, Tokens: tokens})
}

func TestEveryRequestCarriesTheStandardHeaders(t *testing.T) {
	defer i18n.SetLang(i18n.English)
	for _, tc := range []struct{ lang, want string }{{i18n.English, "en"}, {i18n.Spanish, "es-MX"}} {
		i18n.SetLang(tc.lang)
		var got http.Header
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			io.WriteString(w, `{"partner":{"slug":"calisto","name":"Calisto"},"mode":"test"}`)
		}, StaticKey("lst_test_abc"))
		res, err := c.Ping(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Mode != "test" || res.Partner.Slug != "calisto" {
			t.Fatalf("decoded %+v", res)
		}
		if got.Get("Authorization") != "Bearer lst_test_abc" {
			t.Errorf("Authorization = %q", got.Get("Authorization"))
		}
		if got.Get("Accept-Language") != tc.want {
			t.Errorf("Accept-Language = %q, want %q", got.Get("Accept-Language"), tc.want)
		}
		if ua := got.Get("User-Agent"); ua != version.UserAgent() || !regexp.MustCompile(`^listo-cli/\S+ \(\w+/\w+\)$`).MatchString(ua) {
			t.Errorf("User-Agent = %q", ua)
		}
	}
}

func TestErrorEnvelopeBecomesATypedError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"rate_limited","message":"Too many requests.","param":null,"retryable":true,"doc_url":"https://docs.listo.delivery/en/errors/rate_limited","request_id":"req-123"}}`)
	}, StaticKey("lst_test_abc"))
	_, err := c.Ping(context.Background())
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("got %T %v", err, err)
	}
	if ae.Status != 429 || ae.Code != "rate_limited" || ae.Message != "Too many requests." || ae.Param != "" ||
		ae.DocURL != "https://docs.listo.delivery/en/errors/rate_limited" || ae.RequestID != "req-123" || !ae.Retryable ||
		ae.RetryAfter.Seconds() != 7 {
		t.Fatalf("decoded %+v", ae)
	}
	if !errors.Is(err, &Error{Code: "rate_limited"}) || errors.Is(err, &Error{Code: "not_found"}) {
		t.Fatal("errors.Is must match on the code")
	}
	if NextStepKey(ae) != "error.next.rate_limited" {
		t.Fatalf("next step %q", NextStepKey(ae))
	}
}

func TestParamIsDecodedAndANonEnvelopeBodyStillGivesAnError(t *testing.T) {
	n := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(422)
			io.WriteString(w, `{"error":{"code":"validation_failed","message":"bad","param":"dropoff.street","retryable":false,"doc_url":"u","request_id":"r"}}`)
			return
		}
		w.Header().Set("X-Request-Id", "from-header")
		w.WriteHeader(502)
		io.WriteString(w, "<html>bad gateway</html>")
	}, StaticKey("k"))
	_, err := c.Ping(context.Background())
	var ae *Error
	if !errors.As(err, &ae) || ae.Param != "dropoff.street" {
		t.Fatalf("got %v", err)
	}
	_, err = c.Ping(context.Background())
	if !errors.As(err, &ae) || ae.Status != 502 || ae.Code != "" || ae.RequestID != "from-header" {
		t.Fatalf("got %+v", ae)
	}
	if NextStepKey(ae) != "error.next.retry" {
		t.Fatalf("a 5xx with no code still says to retry, got %q", NextStepKey(ae))
	}
}

func TestTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := New(Options{BaseURL: url, Tokens: StaticKey("k")})
	_, err := c.Ping(context.Background())
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("got %T %v", err, err)
	}
}

// A refreshing source: the first token is rejected, the second accepted.
type rotating struct {
	tokens   []string
	i        int
	rejected []string
}

func (r *rotating) Token(context.Context) (string, error) { return r.tokens[r.i], nil }
func (r *rotating) Rejected(_ context.Context, tok string) (bool, error) {
	r.rejected = append(r.rejected, tok)
	if r.i+1 < len(r.tokens) {
		r.i++
		return true, nil
	}
	return false, nil
}

func TestA401IsReportedToTheSourceAndRetriedOnceWithTheSameIdempotencyKey(t *testing.T) {
	var seen []string
	var keys []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"no","param":null,"retryable":false,"doc_url":"u","request_id":"r"}}`)
			return
		}
		w.WriteHeader(201)
		io.WriteString(w, `{"id":"dlv_1"}`)
	}, nil)
	src := &rotating{tokens: []string{"old", "new"}}
	c.tokens = src
	resp, err := c.Do(context.Background(), CreateDelivery, Call{Body: map[string]any{"store_id": "st_1"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "Bearer old,Bearer new" || len(src.rejected) != 1 || src.rejected[0] != "old" {
		t.Fatalf("seen %v rejected %v", seen, src.rejected)
	}
	if keys[0] == "" || keys[0] != keys[1] || resp.IdempotencyKey != keys[0] {
		t.Fatalf("the retry must reuse the key: %v / %q", keys, resp.IdempotencyKey)
	}
}

func TestA401WithNoNewTokenSurfaces(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"no","param":null,"retryable":false,"doc_url":"u","request_id":"r"}}`)
	}, StaticKey("k"))
	_, err := c.Ping(context.Background())
	if !errors.Is(err, &Error{Code: "invalid_api_key", Status: 401}) || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}

func TestIdempotencyKeys(t *testing.T) {
	var key string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		w.WriteHeader(201)
		io.WriteString(w, `{}`)
	}, StaticKey("k"))
	if _, err := c.Do(context.Background(), CreateDelivery, Call{}); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(key) {
		t.Fatalf("minted key %q is not a UUID v4", key)
	}
	first := key
	if _, err := c.Do(context.Background(), CreateDelivery, Call{}); err != nil || key == first {
		t.Fatalf("each call mints its own key (%q twice, %v)", key, err)
	}
	if _, err := c.Do(context.Background(), CreateDelivery, Call{IdempotencyKey: "mine-1"}); err != nil || key != "mine-1" {
		t.Fatalf("an explicit key wins, got %q", key)
	}
	// An endpoint that is not idempotent never sends one.
	if _, err := c.Do(context.Background(), Ping, Call{IdempotencyKey: "x"}); err != nil || key != "" {
		t.Fatalf("a GET must not carry a key, got %q", key)
	}
}

func TestPathQueryAndBodyHandling(t *testing.T) {
	var gotPath, gotQuery, gotBody, gotType string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotType = r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, `{}`)
	}, StaticKey("k"))
	ctx := context.Background()
	if _, err := c.CancelDelivery(ctx, "dlv_a/b", CancelRequest{Reason: "other", Description: "x"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/deliveries/dlv_a%2Fb/cancellations" || gotBody != `{"reason":"other","description":"x"}` || gotType != "application/json" {
		t.Fatalf("path %q body %q type %q", gotPath, gotBody, gotType)
	}
	// A raw body is sent as given, byte for byte.
	if _, err := c.Do(ctx, CreateQuote, Call{Body: json.RawMessage(`{ "store_id" : "st_1" }`)}); err != nil || gotBody != `{ "store_id" : "st_1" }` {
		t.Fatalf("raw body changed: %q %v", gotBody, err)
	}
	// An undeclared query key is refused before anything is sent.
	if _, err := c.Do(ctx, ListEvents, Call{Query: url.Values{"limit": {"5"}}}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("undeclared query parameter accepted: %v", err)
	}
	if _, err := c.Do(ctx, GetDelivery, Call{}); err == nil {
		t.Fatal("a missing path parameter must be an error")
	}
	if _, err := c.Do(ctx, ListEvents, Call{Query: url.Values{"type": {"delivery.updated"}}}); err != nil || gotQuery != "type=delivery.updated" {
		t.Fatalf("query %q %v", gotQuery, err)
	}
}

func TestSendTestWebhookSendsDeliverFalseExplicitly(t *testing.T) {
	var body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"evt_1","type":"delivery.status_changed","created_at":"2026-11-09T12:00:00Z","test":true,"data":{"id":"dlv_x"}}`)
	}, StaticKey("k"))
	no := false
	ev, err := c.SendTestWebhook(context.Background(), TestWebhookRequest{Type: "delivery.status_changed", Deliver: &no})
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"type":"delivery.status_changed","deliver":false}` {
		t.Fatalf("body %q", body)
	}
	if ev.ID != "evt_1" || !ev.Test || !strings.Contains(string(ev.Raw), `"evt_1"`) {
		t.Fatalf("event %+v", ev)
	}
	// Unset means the server's default, so the key is left out.
	if _, err := c.SendTestWebhook(context.Background(), TestWebhookRequest{Type: "store.updated"}); err != nil || body != `{"type":"store.updated"}` {
		t.Fatalf("body %q %v", body, err)
	}
}

func TestPaginationFollowsTheCursorAndKeepsRawEvents(t *testing.T) {
	var cursors []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		cur := r.URL.Query().Get("cursor")
		cursors = append(cursors, cur)
		switch cur {
		case "":
			io.WriteString(w, `{"data":[{"id":"evt_3","type":"delivery.updated","created_at":"c","data":{"k":1}},{"id":"evt_2","type":"delivery.updated","created_at":"b","data":{}}],"has_more":true,"cursor":"c1"}`)
		case "c1":
			io.WriteString(w, `{"data":[{"id":"evt_1","type":"delivery.updated","created_at":"a","data":{}}],"has_more":false,"cursor":null}`)
		default:
			t.Errorf("unexpected cursor %q", cur)
		}
	}, StaticKey("k"))
	var ids []string
	var raw0 string
	err := c.EachEvent(context.Background(), url.Values{"created_after": {"2026-11-09T11:59:00Z"}}, func(e Event) (bool, error) {
		if raw0 == "" {
			raw0 = string(e.Raw)
		}
		ids = append(ids, e.ID)
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "evt_3,evt_2,evt_1" || strings.Join(cursors, "|") != "|c1" {
		t.Fatalf("ids %v cursors %v", ids, cursors)
	}
	if raw0 != `{"id":"evt_3","type":"delivery.updated","created_at":"c","data":{"k":1}}` {
		t.Fatalf("raw item changed: %s", raw0)
	}
}

func TestPaginationStopsEarlyAndOnAStuckCursor(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"data":[{"id":"req_1","method":"GET","path":null,"endpoint":"e","status":200,"error_code":null,"duration_ms":null,"request_id":null,"api_key_prefix":null,"test":true,"created_at":"x"}],"has_more":true,"cursor":"same"}`)
	}, StaticKey("k"))
	n := 0
	err := c.EachRequestLog(context.Background(), nil, func(RequestLog) (bool, error) { n++; return n < 100, nil })
	if err == nil || !strings.Contains(err.Error(), "same cursor") {
		t.Fatalf("a stuck cursor must stop the walk: %v (calls %d)", err, calls)
	}
	n = 0
	err = c.EachRequestLog(context.Background(), nil, func(RequestLog) (bool, error) { n++; return false, nil })
	if err != nil || n != 1 {
		t.Fatalf("returning false must stop: %v %d", err, n)
	}
}

func TestResolveURLs(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name string
		env  map[string]string
		want URLs
	}{
		{"production defaults", nil, URLs{API: "https://api.listo.delivery", Login: "https://login.listo.delivery", Cable: "wss://cable.listo.delivery/cable"}},
		{"workspace", map[string]string{"LISTO_API_URL": "https://api.local.listo.delivery:5012/", "LISTO_CABLE_URL": "wss://api.local.listo.delivery:5012/cable"},
			URLs{API: "https://api.local.listo.delivery:5012", Login: "https://login.local.listo.delivery:5012", Cable: "wss://api.local.listo.delivery:5012/cable"}},
		{"login override wins", map[string]string{"LISTO_API_URL": "https://api.staging.listo.delivery", "LISTO_LOGIN_URL": "https://signin.example.test/"},
			URLs{API: "https://api.staging.listo.delivery", Login: "https://signin.example.test", Cable: "wss://cable.listo.delivery/cable"}},
		{"an api address that does not start with api. has no derivable login host", map[string]string{"LISTO_API_URL": "http://localhost:3000"},
			URLs{API: "http://localhost:3000", Login: "", Cable: "wss://cable.listo.delivery/cable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveURLs(env(tc.env)); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestKeyMode(t *testing.T) {
	for key, want := range map[string]string{"lst_test_abc": "test", "lst_live_abc": "live"} {
		if got, ok := KeyMode(key); !ok || got != want {
			t.Errorf("%s: %q %v", key, got, ok)
		}
	}
	if _, ok := KeyMode("sk_live_x"); ok {
		t.Error("a key with another prefix must not parse")
	}
}

func TestNextStepKeysExistInTheCatalogs(t *testing.T) {
	for _, code := range MappedErrorCodes() {
		key := NextStepKey(&Error{Code: code})
		if key == "" || !i18n.Has(key) {
			t.Errorf("%s: next step key %q is not in the catalog", code, key)
		}
		for _, lang := range []string{i18n.English, i18n.Spanish} {
			if i18n.TIn(lang, key) == key {
				t.Errorf("%s: %q is missing from the %s catalog", code, key, lang)
			}
		}
	}
}
