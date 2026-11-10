package api

import "net/http"

// Endpoint is one operation of the published API the CLI calls. EVERY
// (method, path) the CLI uses is declared in the table below and nowhere else:
// the contract test parses Listo's API definition and fails when a declared
// operation, a query parameter, a body field or an Idempotency-Key header is not
// in the schema, so a rename on the server turns the CLI's CI red.
//
// To use a new operation, add it here (and to All) FIRST, with the query
// parameters and body fields the CLI itself sends.
type Endpoint struct {
	Name   string
	Method string
	Path   string // the schema's spelling, with {name} placeholders filled by Call.Path

	// Query lists the query parameters the CLI may send. Client.Do refuses any
	// other key, so the list cannot drift from the code.
	Query []string

	// Body lists the top-level request fields the CLI ITSELF writes (the
	// contract test holds each to the operation's request component). It does
	// not limit a developer's own JSON file passed through `deliveries create
	// --data`: that body is theirs and the server validates it.
	Body []string

	// Idempotent endpoints carry an Idempotency-Key header, minted per call
	// (a random UUID) unless the caller supplies one.
	Idempotent bool
}

// The operations. Paths are the schema's.
var (
	Ping = Endpoint{Name: "ping", Method: http.MethodGet, Path: "/v1/ping"}

	ListEvents = Endpoint{
		Name: "listEvents", Method: http.MethodGet, Path: "/v1/events",
		Query: []string{"type", "delivery_id", "created_after", "created_before", "cursor"},
	}

	ListRequestLogs = Endpoint{
		Name: "listRequestLogs", Method: http.MethodGet, Path: "/v1/request_logs",
		Query: []string{"status", "endpoint", "request_id", "created_after", "created_before", "cursor"},
	}

	SendTestWebhook = Endpoint{
		Name: "sendTestWebhook", Method: http.MethodPost, Path: "/v1/test_webhooks",
		Body: []string{"type", "deliver"},
	}

	CreateDelivery = Endpoint{
		Name: "createDelivery", Method: http.MethodPost, Path: "/v1/deliveries",
		Body:       []string{"store_id", "quote_id", "external_id", "payment_method", "order_value_cents", "dropoff", "simulate"},
		Idempotent: true,
	}

	GetDelivery = Endpoint{Name: "getDelivery", Method: http.MethodGet, Path: "/v1/deliveries/{id}"}

	CancelDelivery = Endpoint{
		Name: "cancelDelivery", Method: http.MethodPost, Path: "/v1/deliveries/{delivery_id}/cancellations",
		Body: []string{"reason", "description"},
	}

	CreateQuote = Endpoint{
		Name: "createQuote", Method: http.MethodPost, Path: "/v1/quotes",
		// The live `deliveries create` quotes the order it is about to book, so
		// every field of the developer's body that prices a trip is copied onto
		// the quote (store_id, dropoff and payment_method it always sends).
		Body: []string{
			"store_id", "dropoff", "payment_method", "order_value_cents",
			"business_fee_share", "business_fee_cap_cents", "pickup_ready_at", "vehicle", "manifest",
		},
	}

	ListStores = Endpoint{
		Name: "listStores", Method: http.MethodGet, Path: "/v1/stores",
		Query: []string{"cursor"},
	}

	GetStore = Endpoint{Name: "getStore", Method: http.MethodGet, Path: "/v1/stores/{id}"}
)

// All is every operation the CLI uses, in one list for the contract test.
var All = []Endpoint{
	Ping, ListEvents, ListRequestLogs, SendTestWebhook,
	CreateDelivery, GetDelivery, CancelDelivery, CreateQuote, ListStores, GetStore,
}
