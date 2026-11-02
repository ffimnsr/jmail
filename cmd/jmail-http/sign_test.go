package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// assertSignedResponse reads the body and checks the header is the HMAC the
// client can compute from the token.
func assertSignedResponse(t *testing.T, resp *http.Response) []byte {
	t.Helper()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := signResponseBody([]byte(testToken), body)
	if got := resp.Header.Get(responseSignatureHeader); got != want {
		t.Fatalf("%s = %q, want %q for body %q", responseSignatureHeader, got, want, body)
	}
	return body
}

func TestSendSignsResponseBody(t *testing.T) {
	fake := startFakeSMTP(t, "SIG1")
	srv := newTestServer(t, testConfig(fake.addr()))

	resp := postSend(t, srv, validBody, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	body := assertSignedResponse(t, resp)
	if !strings.Contains(string(body), `"status":"queued"`) {
		t.Errorf("body = %q", body)
	}
}

func TestSendSignsErrorBody(t *testing.T) {
	fake := startFakeSMTP(t, "X")
	srv := newTestServer(t, testConfig(fake.addr()))

	resp := postSend(t, srv, `{"from":"evil@other.com","to":["a@example.net"],"text":"y"}`, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	assertSignedResponse(t, resp)
}

func TestSendSignsUnauthorizedResponse(t *testing.T) {
	fake := startFakeSMTP(t, "X")
	srv := newTestServer(t, testConfig(fake.addr()))

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/send", strings.NewReader(validBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	assertSignedResponse(t, resp)
}

func TestSendSignsIdempotentReplay(t *testing.T) {
	fake := startFakeSMTP(t, "SIG2")
	srv := newTestServer(t, testConfig(fake.addr()))

	headers := map[string]string{"Idempotency-Key": "signed-replay"}

	first := postSend(t, srv, validBody, headers)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	firstBody := assertSignedResponse(t, first)

	second := postSend(t, srv, validBody, headers)
	if second.StatusCode != http.StatusAccepted {
		t.Fatalf("second status = %d", second.StatusCode)
	}
	if second.Header.Get("Idempotent-Replay") != "true" {
		t.Error("replay did not set Idempotent-Replay")
	}
	// The replay writes the stored body, so it has to be signed too.
	secondBody := assertSignedResponse(t, second)

	if strings.TrimSpace(string(secondBody)) != strings.TrimSpace(string(firstBody)) {
		t.Errorf("replayed body = %q, want %q", secondBody, firstBody)
	}
}

func TestSignResponseBodyRejectsTamperedBody(t *testing.T) {
	signature := signResponseBody([]byte(testToken), []byte(`{"status":"queued"}`))
	tampered := signResponseBody([]byte(testToken), []byte(`{"status":"queued"} `))
	if signature == tampered {
		t.Fatal("a modified body produced the same signature")
	}
	if signResponseBody([]byte("another-token"), []byte(`{"status":"queued"}`)) == signature {
		t.Fatal("a different token produced the same signature")
	}
}
