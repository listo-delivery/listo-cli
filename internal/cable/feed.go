package cable

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Feed turns the socket's frames into an ORDERED, DE-DUPLICATED stream of items
// and fills the gap a dropped connection leaves (section 4.1 item 6). Both
// `listen` (events) and `logs tail` (request log rows) are a Feed with their own
// item type.
//
// The rules, in the order they matter:
//
//   - The first connect of a session catches up NOTHING: it starts from now.
//   - After a reconnect, subscribe FIRST and BUFFER what arrives, then read the
//     list endpoint (Fetch) from the last seen item's created_at minus Overlap,
//     page by page, and only then release the unseen list items oldest first and
//     the buffer after them. Subscribing first is what leaves no hole between the
//     list read and the live stream; the overlap covers clock skew and a row
//     committed late.
//   - The stream and the list overlap on purpose, so an item is processed ONCE by
//     its id, over a bounded set (SeenLimit ids).
//   - The list read never goes further back than the moment the session started,
//     so a gap that began before anything was seen cannot replay history. The
//     list's created_at is whole seconds, so an item stamped in the start's own
//     second is not "after" the start: it is dropped and its id remembered.
//   - Handle runs on its own goroutine, one item at a time, in order: a slow
//     consumer (a local server that takes seconds) must never stall the read
//     loop, which would miss the ping deadline and look like a dead socket.
type Feed[T any] struct {
	// Decode reads a frame's payload; ok is false for a frame this feed ignores.
	Decode func(payload json.RawMessage) (item T, ok bool)
	// Key is an item's id (the de-duplication key) and its created_at.
	Key func(item T) (id string, created time.Time)
	// Fetch reads the list endpoint newest first, calling each until it returns
	// false or the list ends, for items created at or after `after`.
	Fetch func(ctx context.Context, after time.Time, each func(T) (bool, error)) error
	// Handle runs once per item, in order, on the worker goroutine.
	Handle func(ctx context.Context, item T)
	// CatchUp reports a finished catch-up: how many missed items it released, or
	// the error that stopped it (the stream still goes on). May be nil.
	CatchUp func(missed int, err error)

	Now       func() time.Time // default time.Now
	Overlap   time.Duration    // default 60 seconds
	SeenLimit int              // default 4096
	// MaxCatchUp bounds how many items one catch-up releases (default 2000): an
	// outage of a day must not queue a day of traffic in memory.
	MaxCatchUp int

	mu        sync.Mutex
	started   time.Time
	lastSeen  time.Time
	catching  bool
	gen       int
	buffer    []T
	queue     []T
	wake      chan struct{}
	seen      map[string]struct{}
	seenOrder []string
}

func (f *Feed[T]) defaults() {
	if f.Now == nil {
		f.Now = time.Now
	}
	if f.Overlap == 0 {
		f.Overlap = 60 * time.Second
	}
	if f.SeenLimit == 0 {
		f.SeenLimit = 4096
	}
	if f.MaxCatchUp == 0 {
		f.MaxCatchUp = 2000
	}
	f.wake = make(chan struct{}, 1)
	f.seen = map[string]struct{}{}
}

// Run drives client with this feed until client.Run returns, and returns what it
// returned. It owns the worker goroutine.
func (f *Feed[T]) Run(ctx context.Context, client *Client, cb Callbacks) error {
	f.defaults()
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.work(wctx)
	}()
	// The feed's own bookkeeping first, then the caller's (Ready lines, say).
	catchUp, user := f.subscribed(wctx), cb.Subscribed
	cb.Subscribed = func(first bool) {
		catchUp(first)
		if user != nil {
			user(first)
		}
	}
	cb.Message = f.message
	err := client.Run(ctx, cb)
	cancel()
	<-done
	return err
}

func (f *Feed[T]) work(ctx context.Context) {
	for {
		item, ok := f.next()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-f.wake:
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		id, _ := f.Key(item)
		if !f.markSeen(id) {
			continue
		}
		f.Handle(ctx, item)
	}
}

func (f *Feed[T]) next() (T, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var zero T
	if len(f.queue) == 0 {
		return zero, false
	}
	item := f.queue[0]
	f.queue = f.queue[1:]
	return item, true
}

// markSeen records id and reports whether it was new. The set is bounded: the
// oldest ids are forgotten first.
func (f *Feed[T]) markSeen(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.seen[id]; dup {
		return false
	}
	f.seen[id] = struct{}{}
	f.seenOrder = append(f.seenOrder, id)
	if len(f.seenOrder) > f.SeenLimit {
		delete(f.seen, f.seenOrder[0])
		f.seenOrder = f.seenOrder[1:]
	}
	return true
}

func (f *Feed[T]) signal() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// message runs on the read loop and only queues.
func (f *Feed[T]) message(payload json.RawMessage) {
	item, ok := f.Decode(payload)
	if !ok {
		return
	}
	_, created := f.Key(item)
	f.mu.Lock()
	if created.After(f.lastSeen) {
		f.lastSeen = created
	}
	if f.catching {
		f.buffer = append(f.buffer, item)
	} else {
		f.queue = append(f.queue, item)
	}
	f.mu.Unlock()
	f.signal()
}

func (f *Feed[T]) subscribed(ctx context.Context) func(first bool) {
	return func(first bool) {
		f.mu.Lock()
		if first {
			f.started = f.Now()
			f.mu.Unlock()
			return
		}
		f.gen++
		gen := f.gen
		f.catching = true
		after := f.lastSeen.Add(-f.Overlap)
		if after.Before(f.started) {
			after = f.started
		}
		f.mu.Unlock()
		go f.catchUp(ctx, gen, after)
	}
}

func (f *Feed[T]) catchUp(ctx context.Context, gen int, after time.Time) {
	var missed []T
	// The list reports created_at to the whole second, and created_after compares
	// at that precision, so an item stamped in the second the session started
	// may have been created before it. Nothing created at or before the start is
	// ever a missed item: compare at the list's precision and remember the ids,
	// so a later catch-up that reads the same second never forwards them either.
	f.mu.Lock()
	floor := f.started.Truncate(time.Second)
	f.mu.Unlock()
	err := f.Fetch(ctx, after, func(item T) (bool, error) {
		id, created := f.Key(item)
		if !created.IsZero() && !created.After(floor) {
			if id != "" {
				f.markSeen(id)
			}
			return true, nil
		}
		f.mu.Lock()
		_, dup := f.seen[id]
		f.mu.Unlock()
		if dup {
			return true, nil
		}
		if len(missed) >= f.MaxCatchUp {
			return false, nil
		}
		missed = append(missed, item)
		return true, nil
	})
	if ctx.Err() != nil {
		return
	}

	f.mu.Lock()
	if gen != f.gen {
		// A newer reconnect started its own catch-up and owns the buffer.
		f.mu.Unlock()
		return
	}
	// The list is newest first; release it oldest first.
	// An item the stream already delivered (buffered, or queued and not yet
	// handled) is not "missed": the stream's copy is the one that runs.
	held := map[string]struct{}{}
	for _, it := range f.buffer {
		id, _ := f.Key(it)
		held[id] = struct{}{}
	}
	for _, it := range f.queue {
		id, _ := f.Key(it)
		held[id] = struct{}{}
	}
	released := 0
	for i := len(missed) - 1; i >= 0; i-- {
		if id, _ := f.Key(missed[i]); id != "" {
			if _, dup := held[id]; dup {
				continue
			}
		}
		f.queue = append(f.queue, missed[i])
		released++
		if _, created := f.Key(missed[i]); created.After(f.lastSeen) {
			f.lastSeen = created
		}
	}
	f.queue = append(f.queue, f.buffer...)
	f.buffer = nil
	f.catching = false
	f.mu.Unlock()
	f.signal()
	if f.CatchUp != nil {
		f.CatchUp(released, err)
	}
}
