package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/listo-delivery/listo-cli/internal/cable/cabletest"
	"github.com/listo-delivery/listo-cli/internal/signature"
)

func TestListenNeedsForwardToAndValidatesIt(t *testing.T) {
	env := map[string]string{"LISTO_API_KEY": "lst_test_abc"}
	r := run(t, false, "", env, "listen")
	if r.code != 2 || !strings.Contains(r.err, "forward-to") {
		t.Fatalf("%+v", r)
	}
	r = run(t, false, "", env, "listen", "--forward-to", "localhost:3000")
	if r.code != 2 || !strings.Contains(r.err, "full address") {
		t.Fatalf("%+v", r)
	}
	r = run(t, false, "", env, "listen", "--forward-to", "http://localhost:3000", "--events", "delivery.nope")
	if r.code != 2 || !strings.Contains(r.err, "delivery.nope") || !strings.Contains(r.err, "delivery.status_changed") {
		t.Fatalf("%+v", r)
	}
}

func TestListenPrintSecretIsStableAndAlone(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"LISTO_API_KEY": "lst_test_abc", "LISTO_CONFIG_DIR": dir}
	a := run(t, false, "", env, "listen", "--print-secret")
	b := run(t, false, "", env, "listen", "--print-secret")
	secret := strings.TrimSpace(a.out)
	if a.code != 0 || !strings.HasPrefix(secret, "whsec_") || len(secret) != len("whsec_")+64 || strings.Contains(secret, "\n") {
		t.Fatalf("%+v", a)
	}
	if strings.TrimSpace(b.out) != secret {
		t.Errorf("the secret changed between runs: %q then %q", secret, b.out)
	}
	if a.err != "" {
		t.Errorf("stderr = %q", a.err)
	}
}

func TestListenEndToEndForwardsASignedEventAndEndsWhenTheServerCloses(t *testing.T) {
	body := `{"id":"evt_9","type":"delivery.rated","created_at":"2026-11-09T18:00:00Z","data":{"x":"café"}}`

	var mu sync.Mutex
	var gotBody []byte
	var gotSig string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody, gotSig = b, r.Header.Get(signature.Header)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer local.Close()

	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		_ = c.Event("evt_9", "delivery.rated", body)
		// Wait until the CLI has forwarded it, then revoke.
		for i := 0; i < 400; i++ {
			mu.Lock()
			n := len(gotBody)
			mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = c.Disconnect("server_closing", false)
		c.Hold(time.Second)
	})

	dir := t.TempDir()
	env := map[string]string{
		"LISTO_API_KEY": "lst_test_abc", "LISTO_CONFIG_DIR": dir,
		"LISTO_API_URL": "http://127.0.0.1:1", "LISTO_CABLE_URL": srv.WSURL(),
	}
	secret := strings.TrimSpace(run(t, false, "", env, "listen", "--print-secret").out)
	r := run(t, false, "", env, "listen", "--forward-to", local.URL)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}

	mu.Lock()
	defer mu.Unlock()
	if string(gotBody) != body {
		t.Errorf("forwarded %q", gotBody)
	}
	if !signature.Verify(secret, gotBody, gotSig, time.Now(), 0) {
		t.Errorf("signature does not verify against the secret --print-secret gave")
	}
	// Piped output: one JSON object per line on stdout; the Ready line (with the
	// secret) goes to stderr.
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.out)), &line); err != nil {
		t.Fatalf("stdout is not one JSON object: %q", r.out)
	}
	if line["type"] != "delivery.rated" || line["id"] != "evt_9" || line["status"] != float64(204) {
		t.Errorf("line = %v", line)
	}
	if !strings.Contains(r.err, "Ready [TEST]. Your webhook signing secret is "+secret) {
		t.Errorf("stderr = %q", r.err)
	}
}

func TestListenWithARefusedKeyEndsWithSessionEndedAndExit1(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) { _ = c.Disconnect("unauthorized", false) })
	env := map[string]string{"LISTO_API_KEY": "lst_test_abc", "LISTO_API_URL": "http://127.0.0.1:1", "LISTO_CABLE_URL": srv.WSURL()}
	r := run(t, false, "", env, "listen", "--forward-to", "http://localhost:9")
	if r.code != 1 || !strings.Contains(r.err, "Session ended.") || !strings.Contains(r.err, "listo login") {
		t.Fatalf("%+v", r)
	}
}

// pingAPI answers GET /v1/ping with status (401 carries the error envelope).
func pingAPI(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == 401 {
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"Invalid API key.","param":null,"retryable":false,"doc_url":"https://docs.listo.delivery/en/errors/invalid_api_key","request_id":"req-1"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"partner":{"slug":"calisto","name":"Calisto"},"mode":"test"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestListenAndLogsTailWithARevokedKeyEndWithTheKeyRevokedMessageAndExit1(t *testing.T) {
	for _, tc := range []struct {
		args []string
		next string
		key  string
	}{
		{[]string{"listen", "--forward-to", "http://localhost:9"}, "listo login.", "lst_test_abc"},
		{[]string{"logs", "tail"}, "listo login.", "lst_test_abc"},
		{[]string{"listen", "--live", "--forward-to", "http://localhost:9"}, "listo login --live.", "lst_live_abc"},
	} {
		// The dashboard's revoke: the socket gets reason "remote", reconnect false.
		srv := cabletest.New(t, func(c *cabletest.Conn) {
			_ = c.Handshake()
			_ = c.Disconnect("remote", false)
		})
		env := map[string]string{"LISTO_API_KEY": tc.key, "LISTO_API_URL": pingAPI(t, 401).URL, "LISTO_CABLE_URL": srv.WSURL()}
		r := run(t, false, "", env, tc.args...)
		if r.code != 1 || !strings.Contains(r.err, "The API key was revoked.") || !strings.Contains(r.err, "Create a new") || !strings.Contains(r.err, tc.next) {
			t.Errorf("%v: %+v", tc.args, r)
		}
		if strings.Contains(r.err, "asked not to reconnect") {
			t.Errorf("%v: still prints the clean-close notice: %q", tc.args, r.err)
		}
		if srv.Attempts() != 1 {
			t.Errorf("%v: reconnected a revoked key (%d attempts)", tc.args, srv.Attempts())
		}
	}
}

func TestListenWithARemoteDisconnectOfAWorkingKeyReconnects(t *testing.T) {
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		if c.Index == 0 {
			_ = c.Disconnect("remote", false)
			return
		}
		_ = c.Disconnect("server_closing", false) // a clean end, to finish the run
	})
	env := map[string]string{"LISTO_API_KEY": "lst_test_abc", "LISTO_API_URL": pingAPI(t, 200).URL, "LISTO_CABLE_URL": srv.WSURL()}
	r := run(t, false, "", env, "listen", "--forward-to", "http://localhost:9")
	if r.code != 0 || srv.Attempts() != 2 || !strings.Contains(r.err, "Reconnecting") {
		t.Fatalf("attempts=%d %+v", srv.Attempts(), r)
	}
}

func TestLogsTailValidatesStatus(t *testing.T) {
	r := run(t, false, "", map[string]string{"LISTO_API_KEY": "lst_test_abc"}, "logs", "tail", "--status", "weird")
	if r.code != 2 || !strings.Contains(r.err, "failed or succeeded") {
		t.Fatalf("%+v", r)
	}
}

func TestLogsTailPrintsAJSONObjectPerRow(t *testing.T) {
	row := `{"id":"req_1","method":"POST","path":"/v1/deliveries","endpoint":"api/v1/deliveries#create","status":422,"error_code":"validation_failed","duration_ms":31,"request_id":"rid-1","api_key_prefix":"lst_test_abc","test":true,"created_at":"2026-11-09T18:00:00Z"}`
	srv := cabletest.New(t, func(c *cabletest.Conn) {
		_ = c.Handshake()
		_ = c.SendRaw(`{"identifier":"{\"channel\":\"DeveloperChannel\"}","message":{"type":"request_log","log":` + row + `}}`)
		time.Sleep(100 * time.Millisecond)
		_ = c.Disconnect("server_closing", false)
		c.Hold(time.Second)
	})
	env := map[string]string{"LISTO_API_KEY": "lst_test_abc", "LISTO_API_URL": "http://127.0.0.1:1", "LISTO_CABLE_URL": srv.WSURL()}
	r := run(t, false, "", env, "logs", "tail", "--status", "failed")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.out)), &got); err != nil || got["id"] != "req_1" || got["status"] != float64(422) {
		t.Errorf("stdout = %q", r.out)
	}
	// Terminal output: one readable line.
	r = run(t, true, "", env, "logs", "tail", "--status", "succeeded")
	if r.code != 0 || strings.Contains(r.out, "req_1") {
		t.Errorf("a failed row passed --status succeeded: %+v", r)
	}
}
