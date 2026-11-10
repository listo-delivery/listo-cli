package login

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/config"
	"github.com/listo-delivery/listo-cli/internal/output"
)

// fakeClock advances only when Sleep is called, so Poll runs without waiting.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newClock() *fakeClock { return &fakeClock{now: time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	return ctx.Err()
}

func oauthErr(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": "d"})
}

func okTokens(w http.ResponseWriter, access, refresh string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600, "refresh_token": refresh, "scope": "read write"})
}

// scripted answers each token request with the next entry ("" is success).
func scripted(t *testing.T, answers ...string) (*Flow, *fakeClock, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i >= len(answers) {
			i = len(answers) - 1
		}
		a := answers[i]
		switch {
		case a == "":
			okTokens(w, "lso_at_1", "lso_rt_1")
		case strings.HasPrefix(a, "5"):
			status, _ := strconv.Atoi(a)
			w.WriteHeader(status)
		case a == "429":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
		default:
			oauthErr(w, 400, a)
		}
	}))
	t.Cleanup(srv.Close)
	clock := newClock()
	return &Flow{LoginURL: srv.URL, Now: clock.Now, Sleep: clock.Sleep}, clock, &n
}

func TestPollAnswers(t *testing.T) {
	sec := time.Second
	cases := []struct {
		name    string
		answers []string
		expires int
		wantErr error
		code    string // an OAuthError code expected instead
		sleeps  []time.Duration
		polls   int32
	}{
		{"pending then approved", []string{"authorization_pending", "authorization_pending", ""}, 600, nil, "", []time.Duration{5 * sec, 5 * sec, 5 * sec}, 3},
		{"slow_down adds five seconds, and keeps them", []string{"slow_down", "authorization_pending", ""}, 600, nil, "", []time.Duration{5 * sec, 10 * sec, 10 * sec}, 3},
		{"two slow_downs add ten", []string{"slow_down", "slow_down", ""}, 600, nil, "", []time.Duration{5 * sec, 10 * sec, 15 * sec}, 3},
		{"access_denied", []string{"authorization_pending", "access_denied"}, 600, ErrDenied, "", []time.Duration{5 * sec, 5 * sec}, 2},
		{"expired_token", []string{"expired_token"}, 600, ErrExpired, "", []time.Duration{5 * sec}, 1},
		{"invalid_grant is surfaced as it is", []string{"invalid_grant"}, 600, nil, "invalid_grant", []time.Duration{5 * sec}, 1},
		{"our own clock stops at expires_in", []string{"authorization_pending"}, 12, ErrExpired, "", []time.Duration{5 * sec, 5 * sec, 5 * sec}, 2},
		{"a 429 waits what it asked", []string{"429", ""}, 600, nil, "", []time.Duration{5 * sec, 30 * sec}, 2},
		{"a server error is retried", []string{"503", ""}, 600, nil, "", []time.Duration{5 * sec, 5 * sec}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			flow, clock, polls := scripted(t, c.answers...)
			tok, err := flow.Poll(t.Context(), "lso_dc_X", c.expires, 5)
			switch {
			case c.wantErr != nil:
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err %v, want %v", err, c.wantErr)
				}
			case c.code != "":
				if !errors.Is(err, &OAuthError{Code: c.code}) {
					t.Fatalf("err %v, want %s", err, c.code)
				}
			default:
				if err != nil || tok.AccessToken != "lso_at_1" || tok.RefreshToken != "lso_rt_1" {
					t.Fatalf("%v %+v", err, tok)
				}
				if !tok.ExpiresAt.Equal(clock.Now().Add(time.Hour)) {
					t.Errorf("expires at %v", tok.ExpiresAt)
				}
			}
			if len(clock.sleeps) != len(c.sleeps) {
				t.Fatalf("sleeps %v, want %v", clock.sleeps, c.sleeps)
			}
			for i := range c.sleeps {
				if clock.sleeps[i] != c.sleeps[i] {
					t.Errorf("sleeps %v, want %v", clock.sleeps, c.sleeps)
					break
				}
			}
			if got := atomic.LoadInt32(polls); got != c.polls {
				t.Errorf("polls %d, want %d", got, c.polls)
			}
		})
	}
}

func TestPollGivesUpOnAHostThatStaysDown(t *testing.T) {
	flow, _, polls := scripted(t, "503")
	_, err := flow.Poll(t.Context(), "x", 600, 5)
	var oe *OAuthError
	if !errors.As(err, &oe) || oe.Status != 503 || atomic.LoadInt32(polls) != maxTransient {
		t.Fatalf("%v after %d polls", err, atomic.LoadInt32(polls))
	}
}

func TestPollStopsWhenTheContextIsCanceled(t *testing.T) {
	flow, _, _ := scripted(t, "authorization_pending")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := flow.Poll(ctx, "x", 600, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestDeviceNameIsReadableAndAtMost64Characters(t *testing.T) {
	cases := map[[2]string]string{
		{"ana-mbp.local", "darwin"}: "ana-mbp (macOS)",
		{"DESKTOP-1", "windows"}:    "DESKTOP-1 (Windows)",
		{"box", "linux"}:            "box (Linux)",
		{"", "linux"}:               "computer (Linux)",
		{"box", "plan9"}:            "box (plan9)",
	}
	for in, want := range cases {
		if got := deviceName(in[0], in[1]); got != want {
			t.Errorf("deviceName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
	long := deviceName(strings.Repeat("é", 200), "darwin")
	if n := len([]rune(long)); n != 64 || !strings.HasSuffix(long, " (macOS)") {
		t.Errorf("%d characters: %q", n, long)
	}
	if got := CleanDeviceName("a\x00b\nc"); got != "abc" {
		t.Errorf("%q", got)
	}
}

func TestBrowserCommand(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "open", "linux": "xdg-open", "windows": "rundll32"} {
		if name, args := browserCommand(goos, "https://x/y?a=1&b=2"); name != want || args[len(args)-1] != "https://x/y?a=1&b=2" {
			t.Errorf("%s: %s %v", goos, name, args)
		}
	}
	if err := OpenBrowser("file:///etc/passwd"); err == nil {
		t.Errorf("a file address was opened")
	}
}

// --- the grant source ---

// rotatingServer is the token endpoint's refresh rule: a refresh token works
// once, and one presented again revokes the whole grant.
type rotatingServer struct {
	srv       *httptest.Server
	mu        sync.Mutex
	current   string
	retired   map[string]bool
	revoked   bool
	refreshes int32
	delay     time.Duration
}

func newRotating(t *testing.T) *rotatingServer {
	t.Helper()
	s := &rotatingServer{current: "lso_rt_0", retired: map[string]bool{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		atomic.AddInt32(&s.refreshes, 1)
		time.Sleep(s.delay) // a slow network: the window in which a second terminal would also refresh
		s.mu.Lock()
		defer s.mu.Unlock()
		presented := r.PostForm.Get("refresh_token")
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("client_id") != "listo-cli" {
			oauthErr(w, 400, "invalid_request")
			return
		}
		if s.revoked || s.retired[presented] || presented != s.current {
			if s.retired[presented] {
				s.revoked = true
			}
			oauthErr(w, 400, "invalid_grant")
			return
		}
		s.retired[presented] = true
		n := len(s.retired)
		s.current = "lso_rt_" + strconv.Itoa(n)
		okTokens(w, "lso_at_"+strconv.Itoa(n), s.current)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func storeWith(t *testing.T, access string, expires time.Time) *config.Store {
	t.Helper()
	store, err := config.Open(config.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Save(t.Context(), config.Test, config.Record{
		Credential:    &config.Credential{AccessToken: access, AccessTokenExpiresAt: expires, RefreshToken: "lso_rt_0", DeviceName: "box (Linux)"},
		WebhookSecret: "whsec_keep",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestTokenIsUsedWhileItHasMoreThanAMinuteLeft(t *testing.T) {
	srv := newRotating(t)
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	store := storeWith(t, "lso_at_stored", now.Add(61*time.Second))
	src := &GrantSource{Store: store, Mode: config.Test, Flow: &Flow{LoginURL: srv.srv.URL}, Now: func() time.Time { return now }}
	got, err := src.Token(t.Context())
	if err != nil || got != "lso_at_stored" || atomic.LoadInt32(&srv.refreshes) != 0 {
		t.Fatalf("%q %v after %d refreshes", got, err, srv.refreshes)
	}
	// At 60 seconds or fewer it refreshes.
	src.Now = func() time.Time { return now.Add(2 * time.Second) }
	got, err = src.Token(t.Context())
	if err != nil || got != "lso_at_1" || atomic.LoadInt32(&srv.refreshes) != 1 {
		t.Fatalf("%q %v after %d refreshes", got, err, srv.refreshes)
	}
	rec, _ := store.Load(config.Test)
	if rec.Credential.RefreshToken != "lso_rt_1" || rec.Credential.DeviceName != "box (Linux)" || rec.WebhookSecret != "whsec_keep" {
		t.Errorf("%+v", rec)
	}
	if time.Until(rec.Credential.AccessTokenExpiresAt) < 50*time.Minute {
		t.Errorf("expiry %v", rec.Credential.AccessTokenExpiresAt)
	}
}

// Several sources over ONE store (two terminals), all finding the token stale at
// the same moment, against a server that ends the grant when a rotated-away
// refresh token returns: exactly one refresh request may be made.
func TestRacingRefreshesPresentTheRefreshTokenOnce(t *testing.T) {
	srv := newRotating(t)
	srv.delay = 100 * time.Millisecond
	store := storeWith(t, "lso_at_old", time.Now().Add(-time.Minute))

	const callers = 8
	var wg sync.WaitGroup
	tokens := make([]string, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each caller has its own source and its own handle on the store, as two processes would.
			own, err := config.Open(config.Options{Dir: store.Dir()})
			if err != nil {
				errs[i] = err
				return
			}
			src := &GrantSource{Store: own, Mode: config.Test, Flow: &Flow{LoginURL: srv.srv.URL}}
			<-start
			tokens[i], errs[i] = src.Token(t.Context())
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&srv.refreshes); got != 1 {
		t.Fatalf("%d refresh requests, want 1", got)
	}
	if srv.revoked {
		t.Fatalf("the server revoked the grant: a rotated-away token was presented")
	}
	for i := 0; i < callers; i++ {
		if errs[i] != nil || tokens[i] != "lso_at_1" {
			t.Errorf("caller %d: %q %v", i, tokens[i], errs[i])
		}
	}
}

// The same with the 401 path: every caller was refused the same bearer.
func TestRacingRejectedBearersRefreshOnce(t *testing.T) {
	srv := newRotating(t)
	srv.delay = 100 * time.Millisecond
	store := storeWith(t, "lso_at_old", time.Now().Add(time.Hour)) // looks fresh; the API refused it

	var wg sync.WaitGroup
	errs := make([]error, 6)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			own, _ := config.Open(config.Options{Dir: store.Dir()})
			src := &GrantSource{Store: own, Mode: config.Test, Flow: &Flow{LoginURL: srv.srv.URL}}
			<-start
			retry, err := src.Rejected(t.Context(), "lso_at_old")
			if err == nil && !retry {
				err = errors.New("no retry")
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&srv.refreshes); got != 1 || srv.revoked {
		t.Fatalf("%d refresh requests, revoked %v", got, srv.revoked)
	}
}

func TestInvalidGrantEndsTheSession(t *testing.T) {
	srv := newRotating(t)
	srv.revoked = true
	store := storeWith(t, "lso_at_old", time.Now().Add(-time.Minute))
	src := &GrantSource{Store: store, Mode: config.Test, Flow: &Flow{LoginURL: srv.srv.URL}}
	_, err := src.Token(t.Context())
	var f *output.Failure
	if !errors.As(err, &f) || f.Key != "login.session_ended" || f.Exit != 1 || f.NextKey != "auth.next_login" {
		t.Fatalf("%#v", err)
	}
	rec, _ := store.Load(config.Test)
	if rec.Credential != nil || rec.WebhookSecret != "whsec_keep" {
		t.Errorf("%+v", rec)
	}
	// Nothing stored: no second request.
	before := atomic.LoadInt32(&srv.refreshes)
	if _, err := src.Token(t.Context()); err == nil || atomic.LoadInt32(&srv.refreshes) != before {
		t.Errorf("%v, refreshes %d -> %d", err, before, srv.refreshes)
	}

	// Live names its own next step.
	live := &GrantSource{Store: store, Mode: config.Live, Flow: &Flow{LoginURL: srv.srv.URL}}
	_, err = live.Token(t.Context())
	if !errors.As(err, &f) || f.NextKey != "auth.next_login_live" {
		t.Errorf("%#v", err)
	}
}

func TestANetworkFailureKeepsTheGrant(t *testing.T) {
	srv := newRotating(t)
	store := storeWith(t, "lso_at_old", time.Now().Add(-time.Minute))
	url := srv.srv.URL
	srv.srv.Close()
	src := &GrantSource{Store: store, Mode: config.Test, Flow: &Flow{LoginURL: url}}
	_, err := src.Token(t.Context())
	var te *api.TransportError
	if !errors.As(err, &te) {
		t.Fatalf("%#v", err)
	}
	rec, _ := store.Load(config.Test)
	if rec.Credential == nil || rec.Credential.RefreshToken != "lso_rt_0" {
		t.Errorf("a refresh that never arrived cost the grant: %+v", rec)
	}
}
