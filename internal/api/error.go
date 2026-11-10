package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Error is the API's published error envelope,
// decoded. It is what Do returns for every 4xx and 5xx answer.
//
// Code is the only field to branch on; Message is the server's sentence in the
// language the client asked for (Accept-Language) and is printed as given, never
// matched. A body that is not the envelope (a proxy's HTML page) still gives an
// Error: Status is set and Code and Message are empty.
type Error struct {
	Status     int
	Code       string
	Message    string
	Param      string
	DocURL     string
	RequestID  string
	Retryable  bool
	RetryAfter time.Duration // from the Retry-After header, 0 when absent
}

// Error is a plain description for logs and tests. A person never sees this
// string: output.Printer prints the fields, in the person's language.
func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("api: %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("api: status %d", e.Status)
}

// Is lets callers write errors.Is(err, &api.Error{Code: "read_only_key"}): two
// errors match when the non-empty Code (and Status when given) are equal.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return (t.Code == "" || t.Code == e.Code) && (t.Status == 0 || t.Status == e.Status)
}

// TransportError is a failure to get any answer at all (DNS, refused, timeout).
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// envelope is the wire shape: {"error": {...}}.
type envelope struct {
	Error struct {
		Code      string  `json:"code"`
		Message   string  `json:"message"`
		Param     *string `json:"param"`
		Retryable bool    `json:"retryable"`
		DocURL    string  `json:"doc_url"`
		RequestID string  `json:"request_id"`
	} `json:"error"`
}

func decodeError(status int, header http.Header, body []byte) *Error {
	e := &Error{Status: status, RequestID: header.Get("X-Request-Id")}
	var env envelope
	if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		e.Code = env.Error.Code
		e.Message = env.Error.Message
		if env.Error.Param != nil {
			e.Param = *env.Error.Param
		}
		e.Retryable = env.Error.Retryable
		e.DocURL = env.Error.DocURL
		if env.Error.RequestID != "" {
			e.RequestID = env.Error.RequestID
		}
	}
	if ra := header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil && n > 0 {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return e
}

// Error codes the CLI recognises BY NAME, mapped to the catalog key of the
// next step it tells the person. Every code here must exist in the published
// schema (contract_test.go checks it), and every key must exist in the
// catalogs (error_test.go checks it). A code not listed gets no next step
// beyond what the server's own message says.
var nextSteps = map[string]string{
	"invalid_api_key":            "error.next.invalid_api_key",
	"read_only_key":              "error.next.read_only_key",
	"rate_limited":               "error.next.rate_limited",
	"insufficient_balance":       "error.next.insufficient_balance",
	"plan_required":              "error.next.plan_required",
	"provider_unavailable":       "error.next.retry",
	"internal_error":             "error.next.retry",
	"idempotency_conflict":       "error.next.idempotency_conflict",
	"test_webhook_limit_reached": "error.next.test_webhook_limit",
}

// MappedErrorCodes lists the error codes the client maps by name, sorted. The
// contract test holds each one to the published schema.
func MappedErrorCodes() []string {
	out := make([]string, 0, len(nextSteps))
	for c := range nextSteps {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// NextStepKey returns the catalog key of what to do about e, or "". A 401 with
// a code the table does not know is still told to log in.
func NextStepKey(e *Error) string {
	if k, ok := nextSteps[e.Code]; ok {
		return k
	}
	if e.Status == http.StatusUnauthorized {
		return nextSteps["invalid_api_key"]
	}
	if e.Status >= 500 {
		return nextSteps["internal_error"]
	}
	return ""
}
