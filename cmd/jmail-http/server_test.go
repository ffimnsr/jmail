package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token-0123456789abcdef"

// fakeSMTP is just enough of a submission server for the client to talk to:
// EHLO, MAIL, RCPT, DATA, QUIT, and an optional rejection at RCPT time.
type fakeSMTP struct {
	ln       net.Listener
	queueID  string
	failRCPT bool

	mu   sync.Mutex
	msgs [][]byte
}

func startFakeSMTP(t *testing.T, queueID string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &fakeSMTP{ln: ln, queueID: queueID}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go fake.handle(conn)
		}
	}()
	return fake
}

func (f *fakeSMTP) addr() string { return f.ln.Addr().String() }

func (f *fakeSMTP) messages() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.msgs))
	copy(out, f.msgs)
	return out
}

func (f *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	respond := func(line string) bool {
		if _, err := writer.WriteString(line + "\r\n"); err != nil {
			return false
		}
		return writer.Flush() == nil
	}

	if !respond("220 fake ESMTP ready") {
		return
	}

	for {
		raw, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line := strings.TrimRight(raw, "\r\n")
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "EHLO"):
			if !respond("250-fake greets you") || !respond("250-8BITMIME") || !respond("250 SIZE 10240000") {
				return
			}
		case strings.HasPrefix(upper, "MAIL FROM"):
			if !respond("250 2.1.0 Ok") {
				return
			}
		case strings.HasPrefix(upper, "RCPT TO"):
			if f.failRCPT {
				if !respond("550 5.1.1 No such user") {
					return
				}
				continue
			}
			if !respond("250 2.1.5 Ok") {
				return
			}
		case upper == "DATA":
			if !respond("354 End data with <CR><LF>.<CR><LF>") {
				return
			}
			var body bytes.Buffer
			for {
				data, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if data == ".\r\n" || data == ".\n" {
					break
				}
				// Undo dot-stuffing: exactly one leading dot is removed.
				body.WriteString(strings.TrimPrefix(data, "."))
			}
			f.mu.Lock()
			f.msgs = append(f.msgs, body.Bytes())
			f.mu.Unlock()
			if !respond("250 2.0.0 Ok: queued as " + f.queueID) {
				return
			}
		case upper == "QUIT":
			respond("221 Bye")
			return
		default:
			if !respond("502 5.5.2 Command not recognized") {
				return
			}
		}
	}
}

func testConfig(addr string) *config {
	return &config{
		listen:         "127.0.0.1:0",
		token:          testToken,
		origin:         "example.com",
		domains:        []string{"example.com"},
		hostname:       "mail.example.com",
		smtpAddr:       addr,
		smtpTimeout:    5 * time.Second,
		concurrency:    4,
		allowedFrom:    []string{"example.com"},
		maxBytes:       1 << 20,
		maxRecipients:  10,
		maxAttachments: 5,
		rate:           1000,
		burst:          1000,
		idempotencyTTL: time.Minute,
	}
}

func newTestServer(t *testing.T, cfg *config) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newServer(cfg).routes())
	t.Cleanup(srv.Close)
	return srv
}

func postSend(t *testing.T, srv *httptest.Server, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/send", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeSendResponse(t *testing.T, resp *http.Response) sendResponse {
	t.Helper()
	var body sendResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

func decodeError(t *testing.T, resp *http.Response) errorBody {
	t.Helper()
	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	return body
}

// assertSingleAddress compares the parsed form, so the test does not depend on
// how net/mail chooses to quote a display name.
func assertSingleAddress(t *testing.T, header, wantName, wantAddr string) {
	t.Helper()
	if header == "" {
		t.Fatalf("header is empty, want %q", wantAddr)
	}
	addr, err := mail.ParseAddress(header)
	if err != nil {
		t.Fatalf("parse %q: %v", header, err)
	}
	if addr.Name != wantName || addr.Address != wantAddr {
		t.Errorf("address = %q %q, want %q %q", addr.Name, addr.Address, wantName, wantAddr)
	}
}

const validBody = `{
  "from": "App <noreply@example.com>",
  "to": ["alice@example.net"],
  "cc": ["bob@example.net"],
  "bcc": ["audit@example.org"],
  "reply_to": ["support@example.com"],
  "subject": "Hello",
  "text": "plain",
  "html": "<p>html</p>"
}`

func TestSendQueuesMessage(t *testing.T) {
	fake := startFakeSMTP(t, "4F3A1B2C")
	srv := newTestServer(t, testConfig(fake.addr()))

	resp := postSend(t, srv, validBody, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	body := decodeSendResponse(t, resp)
	if body.ID != "4F3A1B2C" {
		t.Errorf("id = %q, want the Postfix queue id", body.ID)
	}
	if body.Status != "queued" {
		t.Errorf("status = %q, want queued", body.Status)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id header is missing")
	}

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages received = %d, want 1", len(msgs))
	}

	parsed, err := mail.ReadMessage(bytes.NewReader(msgs[0]))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	assertSingleAddress(t, parsed.Header.Get("From"), "App", "noreply@example.com")
	assertSingleAddress(t, parsed.Header.Get("To"), "", "alice@example.net")
	assertSingleAddress(t, parsed.Header.Get("Reply-To"), "", "support@example.com")
	if got := parsed.Header.Get("Bcc"); got != "" {
		t.Errorf("Bcc leaked into headers: %q", got)
	}
	if got := parsed.Header.Get("Message-ID"); !strings.HasSuffix(got, "@mail.example.com>") {
		t.Errorf("Message-ID = %q", got)
	}
	if got := parsed.Header.Get("Subject"); got != "Hello" {
		t.Errorf("Subject = %q", got)
	}
}

func TestSendIsIdempotent(t *testing.T) {
	fake := startFakeSMTP(t, "AAAA1111")
	srv := newTestServer(t, testConfig(fake.addr()))

	key := map[string]string{"Idempotency-Key": "abc-123"}

	first := postSend(t, srv, validBody, key)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	firstBody := decodeSendResponse(t, first)

	second := postSend(t, srv, validBody, key)
	if second.StatusCode != http.StatusAccepted {
		t.Fatalf("second status = %d", second.StatusCode)
	}
	secondBody := decodeSendResponse(t, second)

	if secondBody.ID != firstBody.ID {
		t.Errorf("replayed id = %q, want %q", secondBody.ID, firstBody.ID)
	}
	if second.Header.Get("Idempotent-Replay") != "true" {
		t.Error("replay did not set Idempotent-Replay")
	}
	if got := len(fake.messages()); got != 1 {
		t.Errorf("messages received = %d, want 1", got)
	}
}

func TestSendRequiresToken(t *testing.T) {
	fake := startFakeSMTP(t, "X")
	srv := newTestServer(t, testConfig(fake.addr()))

	for name, header := range map[string]string{
		"missing":      "",
		"wrong":        "Bearer nope",
		"wrong scheme": "Basic " + testToken,
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/send", strings.NewReader(validBody))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}

	if got := len(fake.messages()); got != 0 {
		t.Errorf("messages received = %d, want 0", got)
	}
}

func TestHealthzNeedsNoToken(t *testing.T) {
	fake := startFakeSMTP(t, "X")
	srv := newTestServer(t, testConfig(fake.addr()))

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestSendRejectsForeignSender(t *testing.T) {
	fake := startFakeSMTP(t, "X")
	srv := newTestServer(t, testConfig(fake.addr()))

	body := `{"from":"evil@other.com","to":["a@example.net"],"subject":"x","text":"y"}`
	resp := postSend(t, srv, body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	decoded := decodeError(t, resp)
	if decoded.Error.Code != "sender_not_allowed" {
		t.Errorf("code = %q", decoded.Error.Code)
	}
	if got := len(fake.messages()); got != 0 {
		t.Errorf("messages received = %d, want 0", got)
	}
}

func TestSendAcceptsMarketingHeaders(t *testing.T) {
	fake := startFakeSMTP(t, "MKT1")
	srv := newTestServer(t, testConfig(fake.addr()))

	// The subscription headers required by bulk-send policies are ordinary
	// headers: none of them collide with the reserved names or prefixes.
	headers := map[string]string{
		"List-Unsubscribe":      "<mailto:unsubscribe@example.com?subject=unsubscribe>, <https://example.com/u/42>",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		"List-Id":               "\"Example Newsletter\" <news.example.com>",
		"Precedence":            "bulk",
		"Feedback-ID":           "campaign:spring:example",
	}

	payload := map[string]any{
		"from":    "news@example.com",
		"to":      []string{"alice@example.net"},
		"subject": "Spring news",
		"text":    "hello",
		"headers": headers,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp := postSend(t, srv, string(body), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}

	// Read the message as it was handed to Postfix, which is what OpenDKIM will
	// canonicalise and sign.
	parsed, err := mail.ReadMessage(bytes.NewReader(msgs[0]))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	for name, want := range headers {
		if got := parsed.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestSendRejectsHeaderInjection(t *testing.T) {
	fake := startFakeSMTP(t, "X")
	srv := newTestServer(t, testConfig(fake.addr()))

	cases := map[string]string{
		"subject line break": `{"from":"a@example.com","to":["b@example.net"],"subject":"hi\r\nBcc: evil@example.org","text":"y"}`,
		"reserved header":    `{"from":"a@example.com","to":["b@example.net"],"subject":"hi","text":"y","headers":{"From":"evil@example.org"}}`,
		"header prefix":      `{"from":"a@example.com","to":["b@example.net"],"subject":"hi","text":"y","headers":{"Content-Type":"text/html"}}`,
		"header line break":  `{"from":"a@example.com","to":["b@example.net"],"subject":"hi","text":"y","headers":{"X-Thing":"a\r\nBcc: evil@example.org"}}`,
		"address line break": `{"from":"a@example.com","to":["b@example.net\r\nRCPT TO:<evil@example.org>"],"subject":"hi","text":"y"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := postSend(t, srv, body, nil)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", resp.StatusCode)
			}
		})
	}

	if got := len(fake.messages()); got != 0 {
		t.Errorf("messages received = %d, want 0", got)
	}
}

func TestSendRejectsOversizedBody(t *testing.T) {
	fake := startFakeSMTP(t, "X")
	cfg := testConfig(fake.addr())
	cfg.maxBytes = 128
	srv := newTestServer(t, cfg)

	resp := postSend(t, srv, validBody, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestSendReportsSMTPRejection(t *testing.T) {
	fake := startFakeSMTP(t, "X")
	fake.failRCPT = true
	srv := newTestServer(t, testConfig(fake.addr()))

	resp := postSend(t, srv, validBody, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	decoded := decodeError(t, resp)
	if decoded.Error.Code != "submission_failed" {
		t.Errorf("code = %q", decoded.Error.Code)
	}
	if !strings.Contains(decoded.Error.Message, "550") {
		t.Errorf("message = %q, want the server's reply", decoded.Error.Message)
	}
}

func TestSendRejectsEmptyBody(t *testing.T) {
	fake := startFakeSMTP(t, "X")
	srv := newTestServer(t, testConfig(fake.addr()))

	body := `{"from":"a@example.com","to":["b@example.net"],"subject":"only a subject"}`
	resp := postSend(t, srv, body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestSendUnknownField(t *testing.T) {
	fake := startFakeSMTP(t, "X")
	srv := newTestServer(t, testConfig(fake.addr()))

	body := `{"from":"a@example.com","to":["b@example.net"],"text":"y","body":"typo"}`
	resp := postSend(t, srv, body, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDotStuffing(t *testing.T) {
	fake := startFakeSMTP(t, "DOT1")
	srv := newTestServer(t, testConfig(fake.addr()))

	body := `{"from":"a@example.com","to":["b@example.net"],"subject":"dots","text":"hello\n.dot line\n"}`
	resp := postSend(t, srv, body, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if !bytes.Contains(msgs[0], []byte("\r\n.dot line\r\n")) {
		t.Errorf("stuffed line did not round trip:\n%s", msgs[0])
	}
}

func TestConfigFromPolicy(t *testing.T) {
	signed := []string{"example.com", "second.example"}

	domains, any := parseAllowedFrom("", signed)
	if any || fmt.Sprint(domains) != "[example.com second.example]" {
		t.Fatalf("default = %v, any = %v", domains, any)
	}

	domains, any = parseAllowedFrom(" @Example.com , other.org ", signed)
	if any {
		t.Fatal("unexpected wildcard")
	}
	if fmt.Sprint(domains) != "[example.com other.org]" {
		t.Fatalf("domains = %v", domains)
	}

	if _, any = parseAllowedFrom("*", signed); !any {
		t.Fatal("expected wildcard")
	}
}
