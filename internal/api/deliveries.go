package api

import (
	"context"
	"encoding/json"
	"net/url"
)

// Quote is the part of a POST /v1/quotes answer the CLI reads: the id to book
// and what the trip costs. Money is integer centavos.
type Quote struct {
	ID               string `json:"id"`
	FeeCents         int64  `json:"fee_cents"`
	BusinessFeeCents int64  `json:"business_fee_cents"`
	CustomerFeeCents int64  `json:"customer_fee_cents"`
	Currency         string `json:"currency"`
	ExpiresAt        string `json:"expires_at"`
}

// Store is the part of a store (GET /v1/stores, GET /v1/stores/{id}) the CLI reads.
type Store struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Test bool   `json:"test"`
}

// Delivery is the part of a delivery the CLI draws on a terminal. A command
// that prints JSON prints the object the server sent, not this struct.
type Delivery struct {
	ID               string  `json:"id"`
	ExternalID       *string `json:"external_id"`
	Code             string  `json:"code"`
	Status           string  `json:"status"`
	StoreID          string  `json:"store_id"`
	Test             bool    `json:"test"`
	FeeCents         int64   `json:"fee_cents"`
	BusinessFeeCents int64   `json:"business_fee_cents"`
	CustomerFeeCents int64   `json:"customer_fee_cents"`
	Currency         string  `json:"currency"`
	TrackingURL      *string `json:"tracking_url"`
}

// CreateQuote calls POST /v1/quotes with body as the developer wrote it (the
// fields CreateQuote declares) and returns the quote.
func (c *Client) CreateQuote(ctx context.Context, body any) (*Quote, error) {
	var out Quote
	if _, err := c.DoJSON(ctx, CreateQuote, Call{Body: body}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateDelivery calls POST /v1/deliveries with the developer's own JSON body
// and the Idempotency-Key to send (a minted one when key is empty). It returns
// the key that was sent beside the delivery as the server answered it.
func (c *Client) CreateDelivery(ctx context.Context, body json.RawMessage, key string) (json.RawMessage, string, error) {
	if key == "" {
		key = NewIdempotencyKey()
	}
	resp, err := c.Do(ctx, CreateDelivery, Call{Body: body, IdempotencyKey: key})
	if err != nil {
		return nil, key, err
	}
	return resp.Body, key, nil
}

// GetStore calls GET /v1/stores/{id}.
func (c *Client) GetStore(ctx context.Context, id string) (*Store, error) {
	var out Store
	if _, err := c.DoJSON(ctx, GetStore, Call{Path: map[string]string{"id": id}}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EachStore walks GET /v1/stores newest first (same contract as EachEvent).
func (c *Client) EachStore(ctx context.Context, fn func(Store) (bool, error)) error {
	return Paginate(ctx, c, ListStores, url.Values{}, fn)
}
