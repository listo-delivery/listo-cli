package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
)

// --- a fake API that records what the CLI sent ---

type seenRequest struct {
	method, path, query string
	body                []byte
	header              http.Header
}

type reply struct {
	status int
	body   string
}

type fakeAPI struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	seen   []seenRequest
	routes map[string]reply // "METHOD /path"
}

func newFakeAPI(t *testing.T, routes map[string]reply) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, routes: routes}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, seenRequest{r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Clone()})
		f.mu.Unlock()
		rep, ok := f.routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			rep = reply{404, `{"error":{"code":"not_found","message":"no route"}}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_fake")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

func (f *fakeAPI) sequence() string {
	var parts []string
	for _, r := range f.requests() {
		parts = append(parts, r.method+" "+r.path)
	}
	return strings.Join(parts, ", ")
}

func (f *fakeAPI) only(method, path string) seenRequest {
	f.t.Helper()
	for _, r := range f.requests() {
		if r.method == method && r.path == path {
			return r
		}
	}
	f.t.Fatalf("no %s %s was sent; sent: %s", method, path, f.sequence())
	return seenRequest{}
}

func jsonBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %q (%v)", b, err)
	}
	return m
}

// runIO is run with the output and the input terminals chosen apart, which is
// what the confirmation rules turn on.
func runIO(t *testing.T, outTTY, inTTY bool, in string, env map[string]string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	s := output.Streams{Out: &out, Err: &errb, In: strings.NewReader(in), OutTTY: outTTY, ErrTTY: outTTY, InTTY: inTTY}
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["LISTO_CONFIG_DIR"]; !ok {
		env["LISTO_CONFIG_DIR"] = t.TempDir()
	}
	env["LISTO_NO_KEYRING"] = "1"
	env["NO_COLOR"] = "1"
	code := Execute(args, s, func(k string) string { return env[k] })
	i18n.SetLang(i18n.English)
	return result{code, out.String(), errb.String()}
}

func testEnv(f *fakeAPI) map[string]string {
	return map[string]string{"LISTO_API_URL": f.server.URL, "LISTO_API_KEY": "lst_test_abc", "LANG": "en_US.UTF-8"}
}

func liveEnv(f *fakeAPI) map[string]string {
	return map[string]string{"LISTO_API_URL": f.server.URL, "LISTO_API_KEY": "lst_live_abc", "LANG": "en_US.UTF-8"}
}

const (
	eventJSON    = `{"id":"evt_1","type":"delivery.status_changed","created_at":"2026-11-09T10:00:00Z","test":true,"data":{"id":"dlv_test_sample"}}`
	deliveryJSON = `{"id":"dlv_1","code":"LS-2048","status":"dispatching","store_id":"st_1","test":false,"fee_cents":8900,"business_fee_cents":8900,"customer_fee_cents":0,"currency":"MXN","tracking_url":"https://app.listo.delivery/t/abc"}`
	storeJSON    = `{"id":"st_1","name":"Taquería El Güero","test":false}`
	quoteJSON    = `{"id":"qt_1","fee_cents":8900,"business_fee_cents":8900,"customer_fee_cents":0,"currency":"MXN","expires_at":"2026-11-09T10:05:00Z"}`
	orderJSON    = `{"store_id":"st_1","external_id":"order-841","payment_method":"cash","order_value_cents":19600,"vehicle":"moto","dropoff":{"name":"Ana Torres","phone":"+528112345678","street":"Av. Chapultepec 123, Guadalajara","lat":20.67,"lng":-103.35}}`
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// --- trigger ---

func TestTriggerSendsDeliverFalseByDefault(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"POST /v1/test_webhooks": {202, eventJSON}})
	r := runIO(t, true, true, "", testEnv(f), "trigger", "delivery.status_changed")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	want := "Sent evt_1 (test: true) to your listen sessions. Add --deliver to also POST it to your webhook URL.\n"
	if r.out != want {
		t.Fatalf("stdout %q, want %q", r.out, want)
	}
	req := f.only("POST", "/v1/test_webhooks")
	body := jsonBody(t, req.body)
	if body["type"] != "delivery.status_changed" || body["deliver"] != false || len(body) != 2 {
		t.Fatalf("body %v", body)
	}
	if got := req.header.Get("Authorization"); got != "Bearer lst_test_abc" {
		t.Fatalf("Authorization %q", got)
	}
	if ua := req.header.Get("User-Agent"); !strings.HasPrefix(ua, "listo-cli/") {
		t.Fatalf("User-Agent %q", ua)
	}
	if got := req.header.Get("Accept-Language"); got != "en" {
		t.Fatalf("Accept-Language %q", got)
	}
	if req.header.Get("Idempotency-Key") != "" {
		t.Fatal("test_webhooks is not idempotent and must send no Idempotency-Key")
	}
}

func TestTriggerDeliverSendsTrueAndSaysSo(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"POST /v1/test_webhooks": {202, eventJSON}})
	r := runIO(t, true, true, "", testEnv(f), "trigger", "delivery.status_changed", "--deliver")
	if r.code != 0 || r.out != "Sent evt_1 (test: true) to your listen sessions and queued it for your webhook URL.\n" {
		t.Fatalf("%+v", r)
	}
	if body := jsonBody(t, f.only("POST", "/v1/test_webhooks").body); body["deliver"] != true {
		t.Fatalf("body %v", body)
	}
}

func TestTriggerPrintsTheEventAsJSONWhenPiped(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"POST /v1/test_webhooks": {202, eventJSON}})
	r := runIO(t, false, false, "", testEnv(f), "trigger", "delivery.status_changed")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	ev := jsonBody(t, []byte(r.out))
	if ev["id"] != "evt_1" || ev["test"] != true {
		t.Fatalf("stdout %q", r.out)
	}
	if strings.Contains(r.out, "Sent") {
		t.Fatal("stdout must carry nothing but JSON")
	}
}

func TestTriggerRefusesAnEventTypeTheAPIDoesNotSend(t *testing.T) {
	f := newFakeAPI(t, nil)
	r := runIO(t, true, true, "", testEnv(f), "trigger", "delivery.exploded")
	if r.code != 2 || !strings.Contains(r.err, "'delivery.exploded' is not an event type Listo sends.") {
		t.Fatalf("%+v", r)
	}
	for _, ev := range api.EventTypes {
		if !strings.Contains(r.err, ev) {
			t.Errorf("the usage error does not list %s:\n%s", ev, r.err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("a usage error must not call the API, sent %s", f.sequence())
	}
}

func TestTriggerHelpListsEveryEventTypeInBothLanguages(t *testing.T) {
	for _, lang := range []string{"en", "es"} {
		r := runIO(t, true, true, "", nil, "--lang", lang, "trigger", "--help")
		if r.code != 0 {
			t.Fatalf("%s: %+v", lang, r)
		}
		for _, ev := range api.EventTypes {
			if !strings.Contains(r.out, ev) {
				t.Errorf("[%s] help does not list %s:\n%s", lang, ev, r.out)
			}
		}
		if strings.Contains(r.out, "{list}") {
			t.Errorf("[%s] help leaves the placeholder:\n%s", lang, r.out)
		}
	}
	// The placeholder is put back, so the second run in one process still lists them.
	r := runIO(t, true, true, "", nil, "--lang", "en", "trigger", "--help")
	if !strings.Contains(r.out, "delivery.rated") {
		t.Fatalf("%s", r.out)
	}
}

func TestTriggerLiveAsksBeforeWriting(t *testing.T) {
	routes := map[string]reply{"POST /v1/test_webhooks": {202, eventJSON}}

	t.Run("no terminal and no --yes is exit 2 and sends nothing", func(t *testing.T) {
		f := newFakeAPI(t, routes)
		r := runIO(t, false, false, "y\n", liveEnv(f), "trigger", "delivery.status_changed", "--live", "--deliver")
		if r.code != 2 || !strings.Contains(r.err, "Add --yes.") || len(f.requests()) != 0 {
			t.Fatalf("%+v sent %s", r, f.sequence())
		}
	})
	t.Run("--yes skips the question", func(t *testing.T) {
		f := newFakeAPI(t, routes)
		r := runIO(t, false, false, "", liveEnv(f), "trigger", "delivery.status_changed", "--live", "--deliver", "--yes")
		if r.code != 0 || f.sequence() != "POST /v1/test_webhooks" {
			t.Fatalf("%+v sent %s", r, f.sequence())
		}
		if got := f.only("POST", "/v1/test_webhooks").header.Get("Authorization"); got != "Bearer lst_live_abc" {
			t.Fatalf("Authorization %q", got)
		}
	})
	t.Run("Enter is no", func(t *testing.T) {
		f := newFakeAPI(t, routes)
		r := runIO(t, true, true, "\n", liveEnv(f), "trigger", "delivery.status_changed", "--live", "--deliver")
		if r.code != 0 || !strings.Contains(r.out, "Canceled. Nothing was changed.") || len(f.requests()) != 0 {
			t.Fatalf("%+v sent %s", r, f.sequence())
		}
		if !strings.Contains(r.err, "This will send a test delivery.status_changed event to your live webhook URL. Continue? [y/N]") {
			t.Fatalf("the question: %q", r.err)
		}
	})
	t.Run("y sends", func(t *testing.T) {
		f := newFakeAPI(t, routes)
		r := runIO(t, true, true, "y\n", liveEnv(f), "trigger", "delivery.status_changed", "--live")
		if r.code != 0 || f.sequence() != "POST /v1/test_webhooks" {
			t.Fatalf("%+v sent %s", r, f.sequence())
		}
		if !strings.Contains(r.err, "Nothing is posted to your webhook URL.") {
			t.Fatalf("without --deliver the question says nothing reaches the URL: %q", r.err)
		}
	})
}

func TestTriggerAnswersInSpanish(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"POST /v1/test_webhooks": {202, eventJSON}})
	env := testEnv(f)
	env["LANG"] = "es_MX.UTF-8"
	r := runIO(t, true, true, "", env, "trigger", "delivery.status_changed")
	want := "Se envió evt_1 (test: true) a tus sesiones de listen. Agrega --deliver para enviarlo también por POST a la URL de tu webhook.\n"
	if r.code != 0 || r.out != want {
		t.Fatalf("%+v", r)
	}
	if got := f.only("POST", "/v1/test_webhooks").header.Get("Accept-Language"); got != "es-MX" {
		t.Fatalf("Accept-Language %q", got)
	}
	r = runIO(t, true, true, "", env, "trigger", "nope")
	if r.code != 2 || !strings.Contains(r.err, "'nope' no es un tipo de evento que envíe Listo.") || !strings.Contains(r.err, "Usa uno de: delivery.status_changed") {
		t.Fatalf("%+v", r)
	}
}

func TestTriggerPrintsTheAPIEnvelope(t *testing.T) {
	envelope := `{"error":{"code":"test_webhook_limit_reached","message":"Thirty an hour.","doc_url":"https://docs.listo.delivery/en/errors/test_webhook_limit_reached","retryable":true}}`
	f := newFakeAPI(t, map[string]reply{"POST /v1/test_webhooks": {429, envelope}})
	r := runIO(t, true, true, "", testEnv(f), "trigger", "delivery.status_changed")
	if r.code != 1 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"Error: Thirty an hour.", "Code: test_webhook_limit_reached", "Docs: https://docs.listo.delivery/en/errors/test_webhook_limit_reached", "Request id: req_fake", "Next step: Wait a moment before triggering another test event."} {
		if !strings.Contains(r.err, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.err)
		}
	}
}

// --- deliveries create ---

func TestDeliveriesCreateInTestModeSendsTheDevelopersBodyAsItIs(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"POST /v1/deliveries": {201, deliveryJSON}})
	file := filepath.Join(t.TempDir(), "order.json")
	if err := os.WriteFile(file, []byte(orderJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runIO(t, true, true, "", testEnv(f), "deliveries", "create", "--data", "@"+file)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	req := f.only("POST", "/v1/deliveries")
	if string(req.body) != orderJSON {
		t.Fatalf("the body must reach the API untouched:\n%s", req.body)
	}
	key := req.header.Get("Idempotency-Key")
	if !uuidV4.MatchString(key) {
		t.Fatalf("Idempotency-Key %q is not a minted UUID", key)
	}
	for _, want := range []string{"Idempotency-Key: " + key, "[TEST] Created dlv_1 (LS-2048), status dispatching.", "Delivery fee $89.00 MXN paid by the store.", "Follow it at https://app.listo.delivery/t/abc", "Run listo listen to watch it."} {
		if !strings.Contains(r.out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.out)
		}
	}
	if len(f.requests()) != 1 {
		t.Fatalf("test mode must not quote first, sent %s", f.sequence())
	}
}

func TestDeliveriesCreateReadsStdinAndUsesTheGivenKey(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"POST /v1/deliveries": {201, deliveryJSON}})
	r := runIO(t, false, false, orderJSON+"\n", testEnv(f), "deliveries", "create", "--idempotency-key", "order-841-attempt-1")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	req := f.only("POST", "/v1/deliveries")
	if got := req.header.Get("Idempotency-Key"); got != "order-841-attempt-1" {
		t.Fatalf("Idempotency-Key %q", got)
	}
	if string(req.body) != orderJSON {
		t.Fatalf("body %q", req.body)
	}
	// Piped: the delivery is the only thing on stdout, and the key goes to stderr.
	if d := jsonBody(t, []byte(r.out)); d["id"] != "dlv_1" {
		t.Fatalf("stdout %q", r.out)
	}
	if !strings.Contains(r.err, "Idempotency-Key: order-841-attempt-1") {
		t.Fatalf("the key is printed on stderr: %q", r.err)
	}
}

func TestDeliveriesCreateRefusesAnOrderItCannotRead(t *testing.T) {
	f := newFakeAPI(t, nil)
	cases := []struct {
		name string
		tty  bool
		in   string
		args []string
		want string
	}{
		{"not JSON", false, "{nope", nil, "The order is not a valid JSON object"},
		{"a JSON array", false, "[1]", nil, "The order is not a valid JSON object"},
		{"nothing piped", false, "  \n", nil, "No order was given."},
		{"a terminal and no --data", true, "", nil, "No order was given."},
		{"a missing file", true, "", []string{"--data", "@/no/such/order.json"}, "Could not read /no/such/order.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runIO(t, c.tty, c.tty, c.in, testEnv(f), append([]string{"deliveries", "create"}, c.args...)...)
			if r.code != 2 || !strings.Contains(r.err, c.want) || !strings.Contains(r.err, "next_step") && !strings.Contains(r.err, "Next step:") {
				t.Fatalf("%+v", r)
			}
		})
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("sent %s", f.sequence())
	}
}

func TestDeliveriesCreatePrintsTheAPIEnvelope(t *testing.T) {
	envelope := `{"error":{"code":"validation_failed","message":"dropoff.street is required.","param":"dropoff.street","doc_url":"https://docs.listo.delivery/en/errors/validation_failed"}}`
	f := newFakeAPI(t, map[string]reply{"POST /v1/deliveries": {422, envelope}})
	r := runIO(t, true, true, "", testEnv(f), "deliveries", "create", "--data", orderJSON)
	if r.code != 1 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"Error: dropoff.street is required.", "Code: validation_failed", "Field: dropoff.street", "Request id: req_fake"} {
		if !strings.Contains(r.err, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.err)
		}
	}
}

// --- deliveries create --live ---

func liveRoutes() map[string]reply {
	return map[string]reply{
		"GET /v1/stores/st_1": {200, storeJSON},
		"POST /v1/quotes":     {201, quoteJSON},
		"POST /v1/deliveries": {201, deliveryJSON},
	}
}

const bookSentence = `This will book a courier for "Taquería El Güero" (st_1) to Av. Chapultepec 123, Guadalajara, delivery fee $89.00 MXN paid by the store, from your live balance.`

func TestDeliveriesCreateLiveQuotesThenBooksThatQuoteOnY(t *testing.T) {
	f := newFakeAPI(t, liveRoutes())
	r := runIO(t, true, true, "y\n", liveEnv(f), "deliveries", "create", "--live", "--data", orderJSON)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := f.sequence(); got != "GET /v1/stores/st_1, POST /v1/quotes, POST /v1/deliveries" {
		t.Fatalf("order of calls: %s", got)
	}
	if !strings.Contains(r.err, bookSentence+" Continue? [y/N]") {
		t.Fatalf("the question must be exactly what will happen:\n%s", r.err)
	}

	quote := jsonBody(t, f.only("POST", "/v1/quotes").body)
	if quote["store_id"] != "st_1" || quote["payment_method"] != "cash" || quote["order_value_cents"] != float64(19600) || quote["vehicle"] != "moto" {
		t.Fatalf("quote body %v", quote)
	}
	dropoff, _ := quote["dropoff"].(map[string]any)
	if dropoff["street"] != "Av. Chapultepec 123, Guadalajara" || dropoff["lat"] != 20.67 {
		t.Fatalf("quote dropoff %v", dropoff)
	}
	if _, has := dropoff["name"]; has {
		t.Fatalf("a quote takes no recipient: %v", dropoff)
	}
	if _, has := quote["external_id"]; has {
		t.Fatalf("a quote takes no external_id: %v", quote)
	}

	create := f.only("POST", "/v1/deliveries")
	booked := jsonBody(t, create.body)
	if booked["quote_id"] != "qt_1" || booked["external_id"] != "order-841" || booked["store_id"] != "st_1" {
		t.Fatalf("the delivery must book the quote that was shown: %v", booked)
	}
	if got := create.header.Get("Authorization"); got != "Bearer lst_live_abc" {
		t.Fatalf("Authorization %q", got)
	}
	if !uuidV4.MatchString(create.header.Get("Idempotency-Key")) {
		t.Fatalf("Idempotency-Key %q", create.header.Get("Idempotency-Key"))
	}
}

func TestDeliveriesCreateLiveDoesNothingOnEnter(t *testing.T) {
	f := newFakeAPI(t, liveRoutes())
	r := runIO(t, true, true, "\n", liveEnv(f), "deliveries", "create", "--live", "--data", orderJSON)
	if r.code != 0 || !strings.Contains(r.out, "Canceled. Nothing was changed.") {
		t.Fatalf("%+v", r)
	}
	if got := f.sequence(); strings.Contains(got, "POST /v1/deliveries") {
		t.Fatalf("Enter must book nothing, sent %s", got)
	}
}

func TestDeliveriesCreateLiveWithNoTerminalNeedsYesAndCallsNothing(t *testing.T) {
	f := newFakeAPI(t, liveRoutes())
	r := runIO(t, false, false, orderJSON, liveEnv(f), "deliveries", "create", "--live")
	if r.code != 2 || !strings.Contains(r.err, "there is no terminal to ask on") || !strings.Contains(r.err, "Add --yes.") {
		t.Fatalf("%+v", r)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("must refuse before any call, sent %s", f.sequence())
	}
}

func TestDeliveriesCreateLiveYesPrintsWhatItDoesAndBooksTheQuote(t *testing.T) {
	f := newFakeAPI(t, liveRoutes())
	r := runIO(t, false, false, orderJSON, liveEnv(f), "deliveries", "create", "--live", "--yes")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.err, bookSentence+"\n") || strings.Contains(r.err, "Continue?") {
		t.Fatalf("--yes states what it does and does not ask:\n%s", r.err)
	}
	if booked := jsonBody(t, f.only("POST", "/v1/deliveries").body); booked["quote_id"] != "qt_1" {
		t.Fatalf("%v", booked)
	}
}

func TestDeliveriesCreateLiveSaysWhoPaysTheFee(t *testing.T) {
	cases := []struct {
		name, quote, want string
	}{
		{"the customer", `{"id":"qt_1","fee_cents":8900,"business_fee_cents":0,"customer_fee_cents":8900,"currency":"MXN"}`,
			`delivery fee $89.00 MXN paid by the customer. Continue?`},
		{"split", `{"id":"qt_1","fee_cents":123456,"business_fee_cents":50000,"customer_fee_cents":73456,"currency":"MXN"}`,
			`delivery fee $1,234.56 MXN of which the store pays $500.00 and the customer $734.56, from your live balance. Continue?`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			routes := liveRoutes()
			routes["POST /v1/quotes"] = reply{201, c.quote}
			f := newFakeAPI(t, routes)
			r := runIO(t, true, true, "n\n", liveEnv(f), "deliveries", "create", "--live", "--data", orderJSON)
			if r.code != 0 || !strings.Contains(r.err, c.want) {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestDeliveriesCreateLiveSaysItInSpanish(t *testing.T) {
	f := newFakeAPI(t, liveRoutes())
	env := liveEnv(f)
	env["LANG"] = "es_MX.UTF-8"
	r := runIO(t, true, true, "n\n", env, "deliveries", "create", "--live", "--data", orderJSON)
	want := `Esto reservará un repartidor para "Taquería El Güero" (st_1) hacia Av. Chapultepec 123, Guadalajara, costo de envío $89.00 MXN pagado por la tienda, de tu saldo real. ¿Continuar? [y/N]`
	if r.code != 0 || !strings.Contains(r.err, want) || !strings.Contains(r.out, "Cancelado. No se cambió nada.") {
		t.Fatalf("%+v", r)
	}
}

func TestDeliveriesCreateLiveNeedsAStore(t *testing.T) {
	f := newFakeAPI(t, liveRoutes())
	r := runIO(t, true, true, "y\n", liveEnv(f), "deliveries", "create", "--live", "--data", `{"external_id":"x"}`)
	if r.code != 2 || !strings.Contains(r.err, "The order has no store_id.") || len(f.requests()) != 0 {
		t.Fatalf("%+v sent %s", r, f.sequence())
	}
}

func TestDeliveriesCreateLiveStopsWhenTheQuoteIsRefused(t *testing.T) {
	routes := liveRoutes()
	routes["POST /v1/quotes"] = reply{402, `{"error":{"code":"plan_required","message":"No active plan."}}`}
	f := newFakeAPI(t, routes)
	r := runIO(t, true, true, "y\n", liveEnv(f), "deliveries", "create", "--live", "--data", orderJSON)
	if r.code != 1 || !strings.Contains(r.err, "Code: plan_required") || strings.Contains(f.sequence(), "POST /v1/deliveries") {
		t.Fatalf("%+v sent %s", r, f.sequence())
	}
}

// --- deliveries get and cancel ---

func TestDeliveriesGet(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"GET /v1/deliveries/dlv_1": {200, deliveryJSON}})
	r := runIO(t, true, true, "", testEnv(f), "deliveries", "get", "dlv_1")
	if r.code != 0 || !strings.Contains(r.out, "[TEST] dlv_1 (LS-2048), status dispatching.") || !strings.Contains(r.out, "Delivery fee $89.00 MXN paid by the store.") {
		t.Fatalf("%+v", r)
	}
	if req := f.only("GET", "/v1/deliveries/dlv_1"); len(req.body) != 0 {
		t.Fatalf("a GET has no body: %q", req.body)
	}
	r = runIO(t, false, false, "", testEnv(f), "deliveries", "get", "dlv_1")
	if d := jsonBody(t, []byte(r.out)); r.code != 0 || d["id"] != "dlv_1" {
		t.Fatalf("%+v", r)
	}
	if r := runIO(t, true, true, "", testEnv(f), "deliveries", "get"); r.code != 2 {
		t.Fatalf("a missing id is a usage error: %+v", r)
	}
}

func TestDeliveriesGetPrintsTheAPIEnvelope(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"GET /v1/deliveries/dlv_nope": {404, `{"error":{"code":"not_found","message":"No such delivery.","doc_url":"https://docs.listo.delivery/en/errors/not_found"}}`}})
	r := runIO(t, true, true, "", testEnv(f), "deliveries", "get", "dlv_nope")
	if r.code != 1 || !strings.Contains(r.err, "Error: No such delivery.") || !strings.Contains(r.err, "Code: not_found") {
		t.Fatalf("%+v", r)
	}
}

const canceledJSON = `{"id":"dlv_1","code":"LS-2048","status":"canceled","store_id":"st_1","fee_cents":8900,"business_fee_cents":8900,"customer_fee_cents":0,"currency":"MXN"}`

func TestDeliveriesCancelInTestModeNeedsNoQuestion(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"POST /v1/deliveries/dlv_1/cancellations": {200, canceledJSON}})
	r := runIO(t, false, false, "", testEnv(f), "deliveries", "cancel", "dlv_1", "--reason", "store_closed")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	body := jsonBody(t, f.only("POST", "/v1/deliveries/dlv_1/cancellations").body)
	if body["reason"] != "store_closed" || len(body) != 1 {
		t.Fatalf("body %v", body)
	}

	// With no reason the API still gets a valid one: "other" carries its sentence.
	f = newFakeAPI(t, map[string]reply{"POST /v1/deliveries/dlv_1/cancellations": {200, canceledJSON}})
	r = runIO(t, true, true, "", testEnv(f), "deliveries", "cancel", "dlv_1")
	body = jsonBody(t, f.only("POST", "/v1/deliveries/dlv_1/cancellations").body)
	if r.code != 0 || body["reason"] != "other" || body["description"] != "Canceled with the Listo CLI" {
		t.Fatalf("%+v body %v", r, body)
	}
	if !strings.Contains(r.out, "[TEST] dlv_1 (LS-2048) is now canceled.") {
		t.Fatalf("%s", r.out)
	}
}

func TestDeliveriesCancelRefusesAReasonTheAPIDoesNotTake(t *testing.T) {
	f := newFakeAPI(t, nil)
	r := runIO(t, true, true, "", testEnv(f), "deliveries", "cancel", "dlv_1", "--reason", "bored")
	if r.code != 2 || !strings.Contains(r.err, "'bored' is not a cancel reason.") || len(f.requests()) != 0 {
		t.Fatalf("%+v", r)
	}
	for _, reason := range api.CancelReasons {
		if !strings.Contains(r.err, reason) {
			t.Errorf("does not list %s: %s", reason, r.err)
		}
	}
}

func TestDeliveriesCancelLiveShowsTheDeliveryAndTheChargeThenAsks(t *testing.T) {
	routes := map[string]reply{
		"GET /v1/deliveries/dlv_1":                {200, deliveryJSON},
		"POST /v1/deliveries/dlv_1/cancellations": {200, canceledJSON},
	}
	want := "This will cancel dlv_1 (LS-2048), which is dispatching. Canceling after a courier was already working on the order is billed as a cancellation fee: cancellation charges depend on how far the trip had progressed and are posted to your balance as a separate ledger entry. Continue? [y/N]"

	t.Run("y cancels", func(t *testing.T) {
		f := newFakeAPI(t, routes)
		r := runIO(t, true, true, "y\n", liveEnv(f), "deliveries", "cancel", "dlv_1", "--live", "--reason", "customer_canceled")
		if r.code != 0 || !strings.Contains(r.err, want) || f.sequence() != "GET /v1/deliveries/dlv_1, POST /v1/deliveries/dlv_1/cancellations" {
			t.Fatalf("%+v sent %s", r, f.sequence())
		}
	})
	t.Run("Enter does nothing", func(t *testing.T) {
		f := newFakeAPI(t, routes)
		r := runIO(t, true, true, "\n", liveEnv(f), "deliveries", "cancel", "dlv_1", "--live")
		if r.code != 0 || f.sequence() != "GET /v1/deliveries/dlv_1" || !strings.Contains(r.out, "Canceled. Nothing was changed.") {
			t.Fatalf("%+v sent %s", r, f.sequence())
		}
	})
	t.Run("no terminal and no --yes is exit 2 before any call", func(t *testing.T) {
		f := newFakeAPI(t, routes)
		r := runIO(t, false, false, "", liveEnv(f), "deliveries", "cancel", "dlv_1", "--live")
		if r.code != 2 || !strings.Contains(r.err, "Add --yes.") || len(f.requests()) != 0 {
			t.Fatalf("%+v sent %s", r, f.sequence())
		}
	})
	t.Run("--yes states it and cancels", func(t *testing.T) {
		f := newFakeAPI(t, routes)
		r := runIO(t, false, false, "", liveEnv(f), "deliveries", "cancel", "dlv_1", "--live", "--yes")
		if r.code != 0 || !strings.Contains(r.err, "This will cancel dlv_1 (LS-2048)") || !strings.Contains(f.sequence(), "POST /v1/deliveries/dlv_1/cancellations") {
			t.Fatalf("%+v sent %s", r, f.sequence())
		}
	})
}

// --- simulate ---

func TestSimulateUsesTheFirstTestStoreAndTheQuickstartDropoff(t *testing.T) {
	stores := `{"data":[{"id":"st_new","name":"Newest","test":true},{"id":"st_old","name":"Older","test":true}],"has_more":false,"cursor":null}`
	f := newFakeAPI(t, map[string]reply{
		"GET /v1/stores":      {200, stores},
		"POST /v1/deliveries": {201, `{"id":"dlv_sim","code":"LS-1","status":"dispatching","store_id":"st_new","test":true,"fee_cents":0,"currency":"MXN"}`},
	})
	r := runIO(t, true, true, "", testEnv(f), "simulate", "returned")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.out, "dlv_sim") || !strings.HasSuffix(strings.TrimSpace(r.out), "Run listo listen to watch it.") {
		t.Fatalf("stdout %q", r.out)
	}
	req := f.only("POST", "/v1/deliveries")
	body := jsonBody(t, req.body)
	if body["store_id"] != "st_new" || body["simulate"] != "returned" || body["payment_method"] != "cash" || body["order_value_cents"] != float64(19600) {
		t.Fatalf("body %v", body)
	}
	if ext, _ := body["external_id"].(string); !strings.HasPrefix(ext, "cli-simulate-") {
		t.Fatalf("external_id %v", body["external_id"])
	}
	dropoff, _ := body["dropoff"].(map[string]any)
	if dropoff["name"] != "Ana Torres" || dropoff["phone"] != "+528112345678" || dropoff["street"] != "Blvd. Puerta de Hierro 4965, Zapopan, Jalisco 45116" || dropoff["lat"] != 20.709395 || dropoff["lng"] != -103.413393 {
		t.Fatalf("dropoff %v", dropoff)
	}
	if !uuidV4.MatchString(req.header.Get("Idempotency-Key")) {
		t.Fatalf("Idempotency-Key %q", req.header.Get("Idempotency-Key"))
	}
}

func TestSimulateWithAStoreFlagDoesNotListStores(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"POST /v1/deliveries": {201, deliveryJSON}})
	r := runIO(t, false, false, "", testEnv(f), "simulate", "no_courier", "--store", "st_given")
	if r.code != 0 || f.sequence() != "POST /v1/deliveries" {
		t.Fatalf("%+v sent %s", r, f.sequence())
	}
	body := jsonBody(t, f.only("POST", "/v1/deliveries").body)
	if body["store_id"] != "st_given" || body["simulate"] != "no_courier" {
		t.Fatalf("body %v", body)
	}
	if d := jsonBody(t, []byte(r.out)); d["id"] != "dlv_1" {
		t.Fatalf("piped stdout is the delivery: %q", r.out)
	}
}

func TestSimulateIsRefusedInLiveBeforeAnyCall(t *testing.T) {
	f := newFakeAPI(t, nil)
	r := runIO(t, true, true, "", liveEnv(f), "simulate", "returned", "--live")
	if r.code != 2 || !strings.Contains(r.err, "Simulations only run with a test key") || !strings.Contains(r.err, "Next step: Remove --live.") || len(f.requests()) != 0 {
		t.Fatalf("%+v sent %s", r, f.sequence())
	}
}

func TestSimulateRefusesAnUnknownScenarioAndListsTheRealOnes(t *testing.T) {
	f := newFakeAPI(t, nil)
	r := runIO(t, true, true, "", testEnv(f), "simulate", "explodes")
	if r.code != 2 || !strings.Contains(r.err, "'explodes' is not a scenario.") || len(f.requests()) != 0 {
		t.Fatalf("%+v", r)
	}
	for _, s := range api.SimulateScenarios {
		if !strings.Contains(r.err, s) {
			t.Errorf("does not list %s: %s", s, r.err)
		}
	}
	help := runIO(t, true, true, "", nil, "simulate", "--help")
	for _, s := range api.SimulateScenarios {
		if !strings.Contains(help.out, s) {
			t.Errorf("help does not list %s", s)
		}
	}
}

func TestSimulateWithNoTestStoreSaysWhatToDoNext(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"GET /v1/stores": {200, `{"data":[],"has_more":false,"cursor":null}`}})
	r := runIO(t, true, true, "", testEnv(f), "simulate", "returned")
	if r.code != 1 || !strings.Contains(r.err, "This account has no test store yet.") || !strings.Contains(r.err, "Next step: Create one with POST /v1/stores") {
		t.Fatalf("%+v", r)
	}
	if f.sequence() != "GET /v1/stores" {
		t.Fatalf("sent %s", f.sequence())
	}
}

func TestSimulateAnswersInSpanish(t *testing.T) {
	f := newFakeAPI(t, map[string]reply{"POST /v1/deliveries": {201, deliveryJSON}})
	env := testEnv(f)
	env["LANG"] = "es_MX.UTF-8"
	r := runIO(t, true, true, "", env, "simulate", "returned", "--store", "st_1")
	if r.code != 0 || !strings.Contains(r.out, "Empezó el escenario returned: dlv_1 (dispatching).") || !strings.Contains(r.out, "Corre listo listen para verla en vivo.") {
		t.Fatalf("%+v", r)
	}
}

// --- shared ---

func TestDeliveryCommandsHaveExamplesAndNoBannedCharacters(t *testing.T) {
	for _, lang := range []string{"en", "es"} {
		for _, args := range [][]string{
			{"trigger", "--help"}, {"deliveries", "--help"}, {"deliveries", "create", "--help"},
			{"deliveries", "get", "--help"}, {"deliveries", "cancel", "--help"}, {"simulate", "--help"},
		} {
			r := runIO(t, true, true, "", nil, append([]string{"--lang", lang}, args...)...)
			if r.code != 0 || !strings.Contains(r.out, map[string]string{"en": "Examples:", "es": "Ejemplos:"}[lang]) {
				t.Errorf("%s %v: %+v", lang, args, r)
			}
			for _, bad := range []string{"\u2014", "\u2013", "\u00b7"} {
				if strings.Contains(r.out, bad) {
					t.Errorf("%s %v prints %q", lang, args, bad)
				}
			}
			if strings.Contains(strings.ToLower(r.out), bannedProvider) {
				t.Errorf("%s %v names the provider", lang, args)
			}
		}
	}
}

func TestMoney(t *testing.T) {
	for cents, want := range map[int64]string{0: "$0.00", 5: "$0.05", 8900: "$89.00", 123456: "$1,234.56", 100000000: "$1,000,000.00", -250: "-$2.50"} {
		if got := money(cents); got != want {
			t.Errorf("money(%d) = %q, want %q", cents, got, want)
		}
	}
}
