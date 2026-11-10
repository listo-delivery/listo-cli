// Package cabletest is a fake Action Cable server for the CLI's tests: an
// httptest server that speaks the JSON protocol the real one does (welcome,
// confirm_subscription, ping, data frames, disconnect), with a script per
// connection. It is imported only by tests.
package cabletest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Identifier is the channel identifier the client must subscribe with.
const Identifier = `{"channel":"DeveloperChannel"}`

// Conn is one accepted connection, handed to the script.
type Conn struct {
	T     *testing.T
	Ctx   context.Context
	Raw   *websocket.Conn
	Req   *http.Request
	Index int // 0 for the first accepted connection
}

// Server is the fake. Script runs once per accepted connection, on its own
// goroutine; Gate (optional) runs before the upgrade and may answer the
// request itself by returning true.
type Server struct {
	*httptest.Server
	Script func(c *Conn)
	Gate   func(n int, w http.ResponseWriter, r *http.Request) bool

	mu       sync.Mutex
	attempts int
	accepted int
	headers  []http.Header
}

// New starts a server. Close it with s.Close().
func New(t *testing.T, script func(c *Conn)) *Server {
	t.Helper()
	s := &Server{Script: script}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Server.Close)
	return s
}

// URL is the ws:// address to dial.
func (s *Server) WSURL() string { return "ws" + strings.TrimPrefix(s.Server.URL, "http") + "/cable" }

// Headers are the handshake request headers of every attempt, in order.
func (s *Server) Headers() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

// Attempts is how many handshakes were made (gated or accepted).
func (s *Server) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	n := s.attempts
	s.attempts++
	s.headers = append(s.headers, r.Header.Clone())
	s.mu.Unlock()

	if s.Gate != nil && s.Gate(n, w, r) {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"actioncable-v1-json"}})
	if err != nil {
		return
	}
	s.mu.Lock()
	idx := s.accepted
	s.accepted++
	s.mu.Unlock()
	defer conn.CloseNow()
	s.Script(&Conn{Ctx: r.Context(), Raw: conn, Req: r, Index: idx})
}

func (c *Conn) write(v any) error {
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(c.Ctx, 5*time.Second)
	defer cancel()
	return c.Raw.Write(ctx, websocket.MessageText, b)
}

// Welcome sends the welcome frame.
func (c *Conn) Welcome() error { return c.write(map[string]string{"type": "welcome"}) }

// ExpectSubscribe reads the client's subscribe command and returns its identifier.
func (c *Conn) ExpectSubscribe() (string, error) {
	ctx, cancel := context.WithTimeout(c.Ctx, 5*time.Second)
	defer cancel()
	_, data, err := c.Raw.Read(ctx)
	if err != nil {
		return "", err
	}
	var cmd struct{ Command, Identifier string }
	_ = json.Unmarshal(data, &cmd)
	if cmd.Command != "subscribe" {
		return "", context.Canceled
	}
	return cmd.Identifier, nil
}

// Confirm sends confirm_subscription.
func (c *Conn) Confirm() error {
	return c.write(map[string]string{"type": "confirm_subscription", "identifier": Identifier})
}

// Handshake is Welcome, ExpectSubscribe and Confirm.
func (c *Conn) Handshake() error {
	if err := c.Welcome(); err != nil {
		return err
	}
	if _, err := c.ExpectSubscribe(); err != nil {
		return err
	}
	return c.Confirm()
}

// SendRaw sends text as one frame, unchanged.
func (c *Conn) SendRaw(text string) error {
	ctx, cancel := context.WithTimeout(c.Ctx, 5*time.Second)
	defer cancel()
	return c.Raw.Write(ctx, websocket.MessageText, []byte(text))
}

// Ping sends a ping frame.
func (c *Conn) Ping() error {
	return c.write(map[string]any{"type": "ping", "message": time.Now().Unix()})
}

// Send sends a data frame carrying message.
func (c *Conn) Send(message any) error {
	return c.write(map[string]any{"identifier": Identifier, "message": message})
}

// Event sends an `event` frame.
func (c *Conn) Event(id, eventType, body string) error {
	return c.Send(map[string]string{"type": "event", "id": id, "event_type": eventType, "body": body})
}

// Disconnect sends a disconnect frame.
func (c *Conn) Disconnect(reason string, reconnect bool) error {
	return c.write(map[string]any{"type": "disconnect", "reason": reason, "reconnect": reconnect})
}

// Drop closes the TCP connection without a close handshake.
func (c *Conn) Drop() { c.Raw.CloseNow() }

// Hold blocks until the client goes away or d passes.
func (c *Conn) Hold(d time.Duration) {
	ctx, cancel := context.WithTimeout(c.Ctx, d)
	defer cancel()
	for {
		if _, _, err := c.Raw.Read(ctx); err != nil {
			return
		}
	}
}
