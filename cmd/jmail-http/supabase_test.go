package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testHookSecret = []byte("a-shared-secret-for-tests")

// signWebhook produces the headers Supabase sends, using the same
// HMAC-SHA256-over-<id>.<timestamp>.<body> scheme.
func signWebhook(secret []byte, id string, when time.Time, body []byte) map[string]string {
	timestamp := strconv.FormatInt(when.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return map[string]string{
		"webhook-id":        id,
		"webhook-timestamp": timestamp,
		"webhook-signature": "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)),
	}
}

func supabasePayload(action, email, token, hash string) []byte {
	return []byte(fmt.Sprintf(
		`{"user":{"email":%q},"email_data":{"token":%q,"token_hash":%q,`+
			`"redirect_to":"https://app.example.com/welcome",`+
			`"email_action_type":%q,"site_url":"https://app.example.com"}}`,
		email, token, hash, action))
}

func postHook(t *testing.T, srv *httptest.Server, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+supabaseHookURLPath, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
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

func TestParseWebhookSecret(t *testing.T) {
	raw := []byte("thirty-two-random-bytes-of-secret")
	encoded := base64.StdEncoding.EncodeToString(raw)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"dashboard format", "v1,whsec_" + encoded, string(raw)},
		{"whsec only", "whsec_" + encoded, string(raw)},
		{"bare base64", encoded, string(raw)},
		{"surrounding space", "  v1,whsec_" + encoded + "\n", string(raw)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWebhookSecret(tc.in)
			if err != nil {
				t.Fatalf("parseWebhookSecret: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("secret = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := parseWebhookSecret("not base64!"); err == nil {
		t.Error("a non-base64 secret should be rejected at startup")
	}
}

func TestVerifyWebhook(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	now := time.Unix(1_700_000_000, 0)
	headers := signWebhook(testHookSecret, "msg_123", now, body)

	verify := func(id, ts, sig string, payload []byte, at time.Time) error {
		return verifyWebhook(testHookSecret, id, ts, sig, payload, at)
	}

	t.Run("valid", func(t *testing.T) {
		if err := verify("msg_123", headers["webhook-timestamp"], headers["webhook-signature"], body, now); err != nil {
			t.Fatalf("valid signature rejected: %v", err)
		}
	})

	t.Run("body was modified", func(t *testing.T) {
		err := verify("msg_123", headers["webhook-timestamp"], headers["webhook-signature"], []byte(`{"hello":"there"}`), now)
		if err == nil {
			t.Fatal("a modified body should not verify")
		}
	})

	t.Run("different secret", func(t *testing.T) {
		other := signWebhook([]byte("another-secret"), "msg_123", now, body)
		err := verify("msg_123", other["webhook-timestamp"], other["webhook-signature"], body, now)
		if err == nil {
			t.Fatal("a signature from another secret should not verify")
		}
	})

	t.Run("stale timestamp", func(t *testing.T) {
		err := verify("msg_123", headers["webhook-timestamp"], headers["webhook-signature"], body, now.Add(10*time.Minute))
		if err == nil {
			t.Fatal("a stale timestamp should be rejected to bound replay")
		}
	})

	t.Run("future timestamp", func(t *testing.T) {
		err := verify("msg_123", headers["webhook-timestamp"], headers["webhook-signature"], body, now.Add(-10*time.Minute))
		if err == nil {
			t.Fatal("a timestamp from the future should be rejected")
		}
	})

	t.Run("missing headers", func(t *testing.T) {
		if err := verify("", "", "", body, now); err == nil {
			t.Fatal("missing headers should be rejected")
		}
	})

	t.Run("unknown signature version", func(t *testing.T) {
		sig := strings.TrimPrefix(headers["webhook-signature"], "v1,")
		if err := verify("msg_123", headers["webhook-timestamp"], "v2,"+sig, body, now); err == nil {
			t.Fatal("an unknown signature version should be ignored")
		}
	})
}

func TestSupabaseHookQueuesEmail(t *testing.T) {
	fake := startFakeSMTP(t, "H00K0001")
	cfg := testConfig(fake.addr())
	cfg.supabaseHookSecret = testHookSecret
	cfg.supabaseURL = "https://project.supabase.co"
	cfg.supabaseFrom = "noreply@example.com"
	srv := newTestServer(t, cfg)

	body := supabasePayload("signup", "user@example.net", "305805", "abc123hash")
	resp := postHook(t, srv, body, signWebhook(testHookSecret, "msg_1", time.Now(), body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}

	messages := fake.messages()
	if len(messages) != 1 {
		t.Fatalf("queued %d messages, want 1", len(messages))
	}
	raw := string(messages[0])
	for _, want := range []string{"user@example.net", "Subject: Confirm your email address", "305805"} {
		if !strings.Contains(raw, want) {
			t.Errorf("message does not contain %q:\n%s", want, raw)
		}
	}
}

func TestSupabaseHookRejectsBadSignature(t *testing.T) {
	fake := startFakeSMTP(t, "H00K0002")
	cfg := testConfig(fake.addr())
	cfg.supabaseHookSecret = testHookSecret
	cfg.supabaseURL = "https://project.supabase.co"
	cfg.supabaseFrom = "noreply@example.com"
	srv := newTestServer(t, cfg)

	body := supabasePayload("signup", "user@example.net", "305805", "abc123hash")
	headers := signWebhook(testHookSecret, "msg_2", time.Now(), body)
	// Same signature, different body: the classic tampering case.
	resp := postHook(t, srv, supabasePayload("signup", "attacker@example.net", "305805", "abc123hash"), headers)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.StatusCode, readBody(t, resp))
	}
	if got := len(fake.messages()); got != 0 {
		t.Fatalf("queued %d messages after a bad signature, want 0", got)
	}
}

func TestSupabaseHookIsNotMountedWithoutSecret(t *testing.T) {
	fake := startFakeSMTP(t, "H00K0003")
	srv := newTestServer(t, testConfig(fake.addr()))

	body := supabasePayload("signup", "user@example.net", "305805", "abc123hash")
	resp := postHook(t, srv, body, signWebhook(testHookSecret, "msg_3", time.Now(), body))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the hook is not configured", resp.StatusCode)
	}
}

// A secure email change carries two hashes and the field names are reversed, so
// each address has to get the token that belongs to it.
func TestSupabaseHookSecureEmailChangeSendsTwoEmails(t *testing.T) {
	fake := startFakeSMTP(t, "H00K0004")
	cfg := testConfig(fake.addr())
	cfg.supabaseHookSecret = testHookSecret
	cfg.supabaseURL = "https://project.supabase.co"
	cfg.supabaseFrom = "noreply@example.com"
	srv := newTestServer(t, cfg)

	body := []byte(`{
		"user": {"email": "old@example.net", "new_email": "new@example.net"},
		"email_data": {
			"token": "111111", "token_hash": "hash-for-new-address",
			"token_new": "222222", "token_hash_new": "hash-for-current-address",
			"email_action_type": "email_change", "site_url": "https://app.example.com"
		}
	}`)
	resp := postHook(t, srv, body, signWebhook(testHookSecret, "msg_4", time.Now(), body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}

	messages := fake.messages()
	if len(messages) != 2 {
		t.Fatalf("queued %d messages, want 2", len(messages))
	}

	byRecipient := map[string]string{}
	for _, message := range messages {
		raw := string(message)
		switch {
		case strings.Contains(raw, "old@example.net"):
			byRecipient["old@example.net"] = raw
		case strings.Contains(raw, "new@example.net"):
			byRecipient["new@example.net"] = raw
		}
	}

	current, ok := byRecipient["old@example.net"]
	if !ok {
		t.Fatal("no message for the current address")
	}
	if !strings.Contains(current, "111111") {
		t.Errorf("the current address should get token 111111:\n%s", current)
	}

	updated, ok := byRecipient["new@example.net"]
	if !ok {
		t.Fatal("no message for the new address")
	}
	if !strings.Contains(updated, "222222") {
		t.Errorf("the new address should get token 222222:\n%s", updated)
	}
}

// A hook-only deployment must not accidentally expose the unauthenticated send
// path: without a token that route is not mounted at all.
func TestSendRouteIsNotMountedWithoutToken(t *testing.T) {
	fake := startFakeSMTP(t, "H00K0005")
	cfg := testConfig(fake.addr())
	cfg.token = ""
	cfg.supabaseHookSecret = testHookSecret
	cfg.supabaseFrom = "noreply@example.com"
	srv := newTestServer(t, cfg)

	if resp := postSend(t, srv, validBody, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /v1/send returned %d, want 404 without a token", resp.StatusCode)
	}

	body := supabasePayload("signup", "user@example.net", "305805", "abc123hash")
	if resp := postHook(t, srv, body, signWebhook(testHookSecret, "msg_5", time.Now(), body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("hook returned %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	if got := len(fake.messages()); got != 1 {
		t.Fatalf("queued %d messages, want 1", got)
	}
}

// The link is built here because the payload only carries the site URL, not the
// project API URL.
func TestSupabaseVerifyURL(t *testing.T) {
	cfg := &config{supabaseURL: "https://project.supabase.co"}
	got := cfg.verifyURL("signup", "abc123", "https://app.example.com/welcome")
	want := "https://project.supabase.co/auth/v1/verify?" +
		"redirect_to=https%3A%2F%2Fapp.example.com%2Fwelcome&token=abc123&type=signup"
	if got != want {
		t.Fatalf("verifyURL = %q, want %q", got, want)
	}

	cfg.supabaseURL = ""
	if got := cfg.verifyURL("signup", "abc123", ""); got != "" {
		t.Fatalf("verifyURL without a configured URL = %q, want empty", got)
	}
}

// readBody drains a response body for error messages.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(body)
}
