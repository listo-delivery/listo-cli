// Package api is the CLI's one HTTP client for the Partner Delivery API.
//
// Every command goes through Client.Do (or DoJSON, or one of the typed helpers
// below it), so these hold everywhere:
//
//   - every request carries `User-Agent: listo-cli/<version> (<os>/<arch>)` and
//     `Accept-Language: es-MX` or `en` (the chosen output language), so the
//     server's error messages arrive in the person's language;
//   - the bearer comes from a TokenSource (a static key, or the login lane's
//     grant) and a 401 is reported back to it once, so a refreshed token is
//     retried transparently;
//   - every 4xx and 5xx becomes a *Error carrying code, message, param, doc_url
//     and request_id (a failure to get an answer is a *TransportError);
//   - an operation is used only if it is declared in endpoints.go, which the
//     contract test holds to Listo's API definition.
//
// Typical use from a command:
//
//	c := api.New(api.Options{BaseURL: urls.API, Tokens: tokens})
//	var out api.PingResult
//	_, err := c.DoJSON(ctx, api.Ping, api.Call{}, &out)
//
// Lists are read with EachEvent / EachRequestLog (or Paginate for another
// list), which follow the cursor so a caller never builds one.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/version"
)

// Options configure a Client.
type Options struct {
	BaseURL    string
	Tokens     TokenSource
	HTTPClient *http.Client // default: a client with a 30 second timeout
	UserAgent  string       // default: version.UserAgent()
	// AcceptLanguage returns the Accept-Language to send; default i18n.AcceptLanguage.
	AcceptLanguage func() string
}

// Client calls the API. Build one with New; it is safe for concurrent use.
type Client struct {
	base   string
	tokens TokenSource
	http   *http.Client
	ua     string
	lang   func() string
}

// New builds a Client. BaseURL is required (see ResolveURLs).
func New(o Options) *Client {
	c := &Client{
		base:   strings.TrimRight(o.BaseURL, "/"),
		tokens: o.Tokens,
		http:   o.HTTPClient,
		ua:     o.UserAgent,
		lang:   o.AcceptLanguage,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	if c.ua == "" {
		c.ua = version.UserAgent()
	}
	if c.lang == nil {
		c.lang = i18n.AcceptLanguage
	}
	return c
}

// BaseURL is the API address this client calls.
func (c *Client) BaseURL() string { return c.base }

// Call is one use of an Endpoint.
type Call struct {
	Path  map[string]string // fills the {name} placeholders of the endpoint's path
	Query url.Values        // only keys the endpoint declares
	Body  any               // nil; json.RawMessage or []byte (sent as is); anything else is marshalled

	// IdempotencyKey is sent on an Idempotent endpoint instead of a minted one
	// (listo deliveries create --idempotency-key). Ignored on other endpoints.
	IdempotencyKey string
}

// Response is a successful (2xx) answer.
type Response struct {
	Status         int
	Header         http.Header
	Body           []byte
	RequestID      string // the X-Request-Id header
	IdempotencyKey string // the key sent, "" when the endpoint is not idempotent
}

// NewIdempotencyKey mints a random UUID (version 4) for an Idempotency-Key header.
func NewIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func (c *Client) buildURL(ep Endpoint, call Call) (string, error) {
	path := ep.Path
	for k, v := range call.Path {
		if v == "" {
			return "", fmt.Errorf("api: path parameter %q is empty for %s", k, ep.Name)
		}
		path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(v))
	}
	if strings.Contains(path, "{") {
		return "", fmt.Errorf("api: %s needs path parameters, got %v", ep.Name, call.Path)
	}
	declared := map[string]bool{}
	for _, q := range ep.Query {
		declared[q] = true
	}
	for k := range call.Query {
		if !declared[k] {
			return "", fmt.Errorf("api: query parameter %q is not declared for %s", k, ep.Name)
		}
	}
	u := c.base + path
	if q := call.Query.Encode(); q != "" {
		u += "?" + q
	}
	return u, nil
}

func encodeBody(body any) ([]byte, error) {
	switch b := body.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return b, nil
	case []byte:
		return b, nil
	default:
		return json.Marshal(b)
	}
}

// Do performs the call. A 2xx is a *Response and a nil error; a 4xx or 5xx is a
// *Error; no answer at all is a *TransportError. A 401 is first reported to the
// TokenSource, which may supply a fresh token for one more try.
func (c *Client) Do(ctx context.Context, ep Endpoint, call Call) (*Response, error) {
	if c.tokens == nil {
		return nil, ErrNoCredentials
	}
	target, err := c.buildURL(ep, call)
	if err != nil {
		return nil, err
	}
	payload, err := encodeBody(call.Body)
	if err != nil {
		return nil, err
	}
	key := ""
	if ep.Idempotent {
		key = call.IdempotencyKey
		if key == "" {
			key = NewIdempotencyKey()
		}
	}

	for attempt := 0; ; attempt++ {
		token, err := c.tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := c.send(ctx, ep, target, payload, key, token)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			retry, rerr := c.tokens.Rejected(ctx, token)
			if rerr != nil {
				return nil, rerr
			}
			if retry {
				continue
			}
		}
		return finish(resp, key)
	}
}

type rawResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (c *Client) send(ctx context.Context, ep Endpoint, target string, payload []byte, key, token string) (*rawResponse, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, ep.Method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept-Language", c.lang())
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &TransportError{Err: err}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &TransportError{Err: err}
	}
	return &rawResponse{StatusCode: res.StatusCode, Header: res.Header, Body: data}, nil
}

func finish(r *rawResponse, key string) (*Response, error) {
	if r.StatusCode >= 400 {
		return nil, decodeError(r.StatusCode, r.Header, r.Body)
	}
	return &Response{
		Status: r.StatusCode, Header: r.Header, Body: r.Body,
		RequestID: r.Header.Get("X-Request-Id"), IdempotencyKey: key,
	}, nil
}

// DoJSON is Do plus decoding a 2xx body into out (skipped when out is nil or the
// body is empty).
func (c *Client) DoJSON(ctx context.Context, ep Endpoint, call Call, out any) (*Response, error) {
	resp, err := c.Do(ctx, ep, call)
	if err != nil {
		return nil, err
	}
	if out != nil && len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return resp, fmt.Errorf("api: %s answered a body that is not the documented shape: %w", ep.Name, err)
		}
	}
	return resp, nil
}
