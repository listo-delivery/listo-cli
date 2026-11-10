package cable_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/cable"
	"github.com/listo-delivery/listo-cli/internal/cable/cabletest"
)

type item struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

type feedHarness struct {
	mu      sync.Mutex
	handled []string
	afters  []time.Time
	missed  []int
	errs    []error
}

func (h *feedHarness) list() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.handled...)
}

func (h *feedHarness) fetches() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.afters)
}

func newFeed(h *feedHarness, now time.Time, fetch func(ctx context.Context, after time.Time, each func(item) (bool, error)) error) *cable.Feed[item] {
	return &cable.Feed[item]{
		Decode: func(p json.RawMessage) (item, bool) {
			var it item
			if json.Unmarshal(p, &it) != nil || it.ID == "" {
				return it, false
			}
			return it, true
		},
		Key: func(i item) (string, time.Time) { return i.ID, i.At },
		Fetch: func(ctx context.Context, after time.Time, each func(item) (bool, error)) error {
			h.mu.Lock()
			h.afters = append(h.afters, after)
			h.mu.Unlock()
			return fetch(ctx, after, each)
		},
		Handle: func(_ context.Context, i item) { h.mu.Lock(); h.handled = append(h.handled, i.ID); h.mu.Unlock() },
		CatchUp: func(n int, err error) {
			h.mu.Lock()
			h.missed = append(h.missed, n)
			h.errs = append(h.errs, err)
			h.mu.Unlock()
		},
		Now: func() time.Time { return now },
	}
}

func walk(items []item) func(context.Context, time.Time, func(item) (bool, error)) error {
	return func(_ context.Context, _ time.Time, each func(item) (bool, error)) error {
		for _, i := range items {
			more, err := each(i)
			if err != nil || !more {
				return err
			}
		}
		return nil
	}
}

func startFeed(ctx context.Context, feed *cable.Feed[item], url string) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- feed.Run(ctx, cable.New(fast(url, api.StaticKey("k"))), cable.Callbacks{})
	}()
	return done
}

func TestFeedFirstConnectCatchesUpNothingAndReconnectCatchesUpInOrderWithoutDuplicates(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	mk := func(id string, off time.Duration) item { return item{ID: id, At: t0.Add(off)} }
	e1, e2, e3 := mk("e1", 0), mk("e2", 5*time.Second), mk("e3", 9*time.Second)

	release := make(chan struct{})
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index == 0 {
			_ = c.Send(e1)
			time.Sleep(50 * time.Millisecond)
			c.Drop()
			return
		}
		// Arrives while the catch-up is still reading the list: it must be
		// buffered and come out AFTER the list's items.
		_ = c.Send(e3)
		c.Hold(3 * time.Second)
	})

	h := &feedHarness{}
	// The list is newest first and holds all three: e1 was seen, e3 is also in
	// the buffer, only e2 is missing.
	feed := newFeed(h, t0.Add(-time.Hour), func(ctx context.Context, after time.Time, each func(item) (bool, error)) error {
		<-release
		return walk([]item{e3, e2, e1})(ctx, after, each)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startFeed(ctx, feed, srv.WSURL())

	within(t, 3*time.Second, "e1", func() bool { return len(h.list()) == 1 })
	within(t, 3*time.Second, "the catch-up to start", func() bool { return h.fetches() == 1 })
	time.Sleep(100 * time.Millisecond) // e3 has had time to arrive on the new socket
	if got := h.list(); len(got) != 1 {
		t.Fatalf("e3 was handled before the catch-up finished (buffering is broken): %v", got)
	}
	close(release)
	within(t, 3*time.Second, "all three", func() bool { return len(h.list()) == 3 })
	time.Sleep(100 * time.Millisecond) // a duplicate would show up here
	cancel()
	<-done

	if got := h.list(); len(got) != 3 || got[0] != "e1" || got[1] != "e2" || got[2] != "e3" {
		t.Errorf("handled = %v, want [e1 e2 e3] once each, in order", got)
	}
	// One list read, from the last seen created_at minus 60 seconds.
	if len(h.afters) != 1 || !h.afters[0].Equal(t0.Add(-60*time.Second)) {
		t.Errorf("after = %v, want %v", h.afters, t0.Add(-60*time.Second))
	}
	if len(h.missed) != 1 || h.missed[0] != 1 {
		t.Errorf("missed = %v, want [1] (only e2 was missing from the list)", h.missed)
	}
}

func TestFeedFirstConnectReadsNoList(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		_ = c.Send(item{ID: "x", At: time.Now()})
		c.Hold(time.Second)
	})
	h := &feedHarness{}
	feed := newFeed(h, time.Now(), walk(nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startFeed(ctx, feed, srv.WSURL())
	within(t, 3*time.Second, "x", func() bool { return len(h.list()) == 1 })
	cancel()
	<-done
	if h.fetches() != 0 {
		t.Errorf("the first connect read the list %d times, want 0 (it starts from now)", h.fetches())
	}
}

func TestFeedNeverReadsBeforeTheSessionStarted(t *testing.T) {
	started := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index == 0 {
			time.Sleep(30 * time.Millisecond)
			c.Drop()
			return
		}
		c.Hold(3 * time.Second)
	})
	h := &feedHarness{}
	feed := newFeed(h, started, walk(nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startFeed(ctx, feed, srv.WSURL())
	within(t, 3*time.Second, "a catch-up", func() bool { return h.fetches() == 1 })
	cancel()
	<-done
	if !h.afters[0].Equal(started) {
		t.Errorf("after = %v, want the session start %v (nothing seen yet, nothing before it)", h.afters[0], started)
	}
}

func TestFeedCatchUpFailureIsReportedAndTheStreamGoesOn(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index == 0 {
			c.Drop()
			return
		}
		_ = c.Send(item{ID: "live", At: t0})
		c.Hold(3 * time.Second)
	})
	h := &feedHarness{}
	boom := errors.New("api down")
	feed := newFeed(h, t0.Add(-time.Hour), func(context.Context, time.Time, func(item) (bool, error)) error { return boom })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startFeed(ctx, feed, srv.WSURL())
	within(t, 3*time.Second, "the live item", func() bool { return len(h.list()) == 1 })
	cancel()
	<-done
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.errs) != 1 || !errors.Is(h.errs[0], boom) {
		t.Errorf("errs = %v", h.errs)
	}
}

func TestFeedSeenSetIsBounded(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		for _, id := range []string{"a", "b", "a", "c", "a"} {
			_ = c.Send(item{ID: id, At: t0})
		}
		c.Hold(3 * time.Second)
	})
	h := &feedHarness{}
	feed := newFeed(h, t0, walk(nil))
	feed.SeenLimit = 2 // the second "a" is a duplicate; after "c" pushes "a" out, the last one is new again
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startFeed(ctx, feed, srv.WSURL())
	within(t, 3*time.Second, "four distinct handles", func() bool { return len(h.list()) == 4 })
	cancel()
	<-done
	if got := h.list(); got[0] != "a" || got[1] != "b" || got[2] != "c" || got[3] != "a" {
		t.Errorf("handled = %v, want [a b c a]", got)
	}
}

func TestFeedCatchUpNeverForwardsAnItemFromTheSecondTheSessionStarted(t *testing.T) {
	// The session starts mid-second. The list reports created_at to the whole
	// second, so "before" (created at 12:00:00.1, listed as 12:00:00) comes back
	// from created_after=12:00:00 although it predates the start.
	start := time.Date(2026, 11, 9, 12, 0, 0, 400_000_000, time.UTC)
	sec := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	before := item{ID: "before", At: sec}
	older := item{ID: "older", At: sec.Add(-time.Second)}
	missed := item{ID: "missed", At: sec.Add(3 * time.Second)}

	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index < 2 { // two reconnects: the second must not forward it either
			time.Sleep(30 * time.Millisecond)
			c.Drop()
			return
		}
		c.Hold(3 * time.Second)
	})
	h := &feedHarness{}
	feed := newFeed(h, start, walk([]item{missed, before, older})) // newest first
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startFeed(ctx, feed, srv.WSURL())
	within(t, 5*time.Second, "two catch-ups", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.missed) == 2 })
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if got := h.list(); len(got) != 1 || got[0] != "missed" {
		t.Errorf("handled = %v, want only [missed] (nothing created at or before the start, over two catch-ups)", got)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.missed[0] != 1 || h.missed[1] != 0 {
		t.Errorf("released per catch-up = %v, want [1 0]", h.missed)
	}
}
