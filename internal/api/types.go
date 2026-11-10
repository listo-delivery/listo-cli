package api

import (
	"context"
	"encoding/json"
	"net/url"
)

// PingResult is GET /v1/ping: which company the key belongs to and its world.
type PingResult struct {
	Partner struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"partner"`
	Mode string `json:"mode"` // "test" or "live"
}

// Ping calls GET /v1/ping (`listo whoami`).
func (c *Client) Ping(ctx context.Context) (*PingResult, error) {
	var out PingResult
	if _, err := c.DoJSON(ctx, Ping, Call{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Event is one webhook event as GET /v1/events lists it and as POST
// /v1/test_webhooks answers it: the envelope your endpoint is sent.
//
// Raw is the item exactly as the server sent it, which is what a catch-up
// forwards (and signs); the other fields are for display and de-duplication.
// A real event has no "test" key; a synthetic one carries test: true.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt string          `json:"created_at"`
	Test      bool            `json:"test"`
	Data      json.RawMessage `json:"data"`
	Raw       json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the raw bytes beside the decoded fields.
func (e *Event) UnmarshalJSON(b []byte) error {
	type plain Event
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*e = Event(p)
	e.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// RequestLog is one row of GET /v1/request_logs (no bodies).
type RequestLog struct {
	ID           string  `json:"id"`
	Method       string  `json:"method"`
	Path         *string `json:"path"`
	Endpoint     string  `json:"endpoint"`
	Status       int     `json:"status"`
	ErrorCode    *string `json:"error_code"`
	DurationMS   *int    `json:"duration_ms"`
	RequestID    *string `json:"request_id"`
	APIKeyPrefix *string `json:"api_key_prefix"`
	Test         bool    `json:"test"`
	CreatedAt    string  `json:"created_at"`
}

// TestWebhookRequest is the body of POST /v1/test_webhooks. Type is one of
// EventTypes. Deliver is a pointer so "false" is sent and "unset" is not
// (the server's default is true): `listo trigger` sends false unless --deliver.
type TestWebhookRequest struct {
	Type    string `json:"type"`
	Deliver *bool  `json:"deliver,omitempty"`
}

// SendTestWebhook calls POST /v1/test_webhooks (`listo trigger`) and returns the
// synthetic event it recorded.
func (c *Client) SendTestWebhook(ctx context.Context, req TestWebhookRequest) (*Event, error) {
	var out Event
	if _, err := c.DoJSON(ctx, SendTestWebhook, Call{Body: req}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelRequest is the body of POST /v1/deliveries/{delivery_id}/cancellations.
// Reason is one of the schema's enum (customer_canceled, store_closed,
// item_unavailable, wrong_address, no_longer_needed, other); Description is
// required when Reason is "other".
type CancelRequest struct {
	Reason      string `json:"reason"`
	Description string `json:"description,omitempty"`
}

// GetDelivery reads GET /v1/deliveries/{id} and returns the object as sent.
func (c *Client) GetDelivery(ctx context.Context, id string) (json.RawMessage, error) {
	resp, err := c.Do(ctx, GetDelivery, Call{Path: map[string]string{"id": id}})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// CancelDelivery calls the cancel route and returns the delivery as sent.
func (c *Client) CancelDelivery(ctx context.Context, id string, req CancelRequest) (json.RawMessage, error) {
	resp, err := c.Do(ctx, CancelDelivery, Call{Path: map[string]string{"delivery_id": id}, Body: req})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// EachEvent walks GET /v1/events newest first, following the cursor, calling fn
// for each event until fn returns false or the list ends. q may hold type,
// delivery_id, created_after and created_before. For a catch-up, collect the
// events and forward them oldest first.
func (c *Client) EachEvent(ctx context.Context, q url.Values, fn func(Event) (bool, error)) error {
	return Paginate(ctx, c, ListEvents, q, fn)
}

// EachRequestLog walks GET /v1/request_logs newest first (same contract).
func (c *Client) EachRequestLog(ctx context.Context, q url.Values, fn func(RequestLog) (bool, error)) error {
	return Paginate(ctx, c, ListRequestLogs, q, fn)
}
