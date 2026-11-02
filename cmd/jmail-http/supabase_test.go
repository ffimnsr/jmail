package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

// addTestHook registers one Supabase project on c. Each project's name is
// unique, the way the environment prefixes make it in loadConfig.
func addTestHook(c *config, secret []byte, apiURL, from string) {
	name := "test"
	if n := len(c.supabaseHooks); n > 0 {
		name = fmt.Sprintf("test-%d", n+1)
	}
	c.supabaseHooks = append(c.supabaseHooks, supabaseHook{
		name:   name,
		secret: secret,
		apiURL: apiURL,
		from:   from,
	})
}

// Each project signs with its own secret, and the matching one decides the
// project URL and sender for that message.
func TestSupabaseHookSelectsProjectBySecret(t *testing.T) {
	fake := startFakeSMTP(t, "H00K0006")
	cfg := testConfig(fake.addr())
	addTestHook(cfg, []byte("secret-one"), "https://one.supabase.co", "one@example.com")
	addTestHook(cfg, []byte("secret-two"), "https://two.supabase.co", "two@example.com")
	srv := newTestServer(t, cfg)

	body := supabasePayload("recovery", "user@example.net", "654321", "hash-two")
	resp := postHook(t, srv, body, signWebhook([]byte("secret-two"), "msg_6", time.Now(), body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}

	messages := fake.messages()
	if len(messages) != 1 {
		t.Fatalf("queued %d messages, want 1", len(messages))
	}
	raw := string(messages[0])
	for _, want := range []string{"two@example.com", "two.supabase.co", "654321"} {
		if !strings.Contains(raw, want) {
			t.Errorf("message does not contain %q:\n%s", want, raw)
		}
	}
	for _, unwanted := range []string{"one@example.com", "one.supabase.co"} {
		if strings.Contains(raw, unwanted) {
			t.Errorf("message used the other project's %q:\n%s", unwanted, raw)
		}
	}
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
	addTestHook(cfg, testHookSecret, "https://project.supabase.co", "noreply@example.com")
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
	for _, want := range []string{"user@example.net", "Subject: Confirm your email address", "From: \"Example\" <noreply@example.com>", "305805", "Example", "Confirm email address", "text/html"} {
		if !strings.Contains(raw, want) {
			t.Errorf("message does not contain %q:\n%s", want, raw)
		}
	}
}

func TestSupabaseHookRejectsBadSignature(t *testing.T) {
	fake := startFakeSMTP(t, "H00K0002")
	cfg := testConfig(fake.addr())
	addTestHook(cfg, testHookSecret, "https://project.supabase.co", "noreply@example.com")
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
	addTestHook(cfg, testHookSecret, "https://project.supabase.co", "noreply@example.com")
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

// A secure email change is two messages. If the second fails after the first was
// queued, the retry must resume rather than sending the first again.
func TestSupabaseHookRetryAfterPartialEmailChange(t *testing.T) {
	fake := startFakeSMTP(t, "H00KPART")
	cfg := testConfig(fake.addr())
	// No API URL, so a message with neither a code nor a link is rejected.
	addTestHook(cfg, testHookSecret, "", "noreply@example.com")
	srv := newTestServer(t, cfg)

	first := []byte(`{
		"user": {"email": "old@example.net", "new_email": "new@example.net"},
		"email_data": {
			"token": "111111", "token_hash": "hash-for-new",
			"token_new": "", "token_hash_new": "hash-for-old",
			"email_action_type": "email_change"
		}
	}`)
	resp := postHook(t, srv, first, signWebhook(testHookSecret, "msg_partial", time.Now(), first))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("first status = %d, want 422: %s", resp.StatusCode, readBody(t, resp))
	}

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("queued %d messages before the failure, want 1", len(msgs))
	}
	if !strings.Contains(string(msgs[0]), "111111") {
		t.Fatalf("the first message should carry 111111:\n%s", msgs[0])
	}

	// The same event, but now the second message has a code to send.
	second := []byte(`{
		"user": {"email": "old@example.net", "new_email": "new@example.net"},
		"email_data": {
			"token": "111111", "token_hash": "hash-for-new",
			"token_new": "222222", "token_hash_new": "hash-for-old",
			"email_action_type": "email_change"
		}
	}`)
	resp = postHook(t, srv, second, signWebhook(testHookSecret, "msg_partial", time.Now(), second))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}

	msgs = fake.messages()
	if len(msgs) != 2 {
		t.Fatalf("queued %d messages after the retry, want 2", len(msgs))
	}

	var toOld, toNew int
	for _, msg := range msgs {
		raw := string(msg)
		switch {
		case strings.Contains(raw, "old@example.net"):
			toOld++
			if !strings.Contains(raw, "111111") {
				t.Errorf("the message to the current address is missing 111111:\n%s", raw)
			}
		case strings.Contains(raw, "new@example.net"):
			toNew++
			if !strings.Contains(raw, "222222") {
				t.Errorf("the message to the new address is missing 222222:\n%s", raw)
			}
		}
	}
	if toOld != 1 || toNew != 1 {
		t.Fatalf("messages to current/new = %d/%d, want 1/1", toOld, toNew)
	}
}

// A hook-only deployment must not accidentally expose the unauthenticated send
// path: without a token that route is not mounted at all.
func TestSendRouteIsNotMountedWithoutToken(t *testing.T) {
	fake := startFakeSMTP(t, "H00K0005")
	cfg := testConfig(fake.addr())
	cfg.token = ""
	addTestHook(cfg, testHookSecret, "", "noreply@example.com")
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
	hook := supabaseHook{apiURL: "https://project.supabase.co"}
	got := hook.verifyURL("signup", "abc123", "https://app.example.com/welcome")
	want := "https://project.supabase.co/auth/v1/verify?" +
		"redirect_to=https%3A%2F%2Fapp.example.com%2Fwelcome&token=abc123&type=signup"
	if got != want {
		t.Fatalf("verifyURL = %q, want %q", got, want)
	}

	hook.apiURL = ""
	if got := hook.verifyURL("signup", "abc123", ""); got != "" {
		t.Fatalf("verifyURL without a configured URL = %q, want empty", got)
	}
}

// supabasePayloadWithData starts from the normal payload shape and adds the
// fields a notification carries.
func supabasePayloadWithData(action, email string, fields map[string]string) []byte {
	emailData := map[string]string{"email_action_type": action}
	for key, value := range fields {
		emailData[key] = value
	}
	body, err := json.Marshal(map[string]any{
		"user":       map[string]string{"email": email},
		"email_data": emailData,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// Reauthentication is an OTP flow. GoTrue does send a token hash, but no public
// endpoint accepts it for this action, so the email must carry the code only.
func TestSupabaseHookReauthenticationSendsCodeOnly(t *testing.T) {
	fake := startFakeSMTP(t, "H00KREAU")
	cfg := testConfig(fake.addr())
	addTestHook(cfg, testHookSecret, "https://project.supabase.co", "noreply@example.com")
	srv := newTestServer(t, cfg)

	body := supabasePayload("reauthentication", "user@example.net", "123456", "reauth-hash")
	resp := postHook(t, srv, body, signWebhook(testHookSecret, "msg_reauth", time.Now(), body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}

	messages := fake.messages()
	if len(messages) != 1 {
		t.Fatalf("queued %d messages, want 1", len(messages))
	}
	raw := string(messages[0])
	if !strings.Contains(raw, "123456") {
		t.Errorf("message does not carry the OTP:\n%s", raw)
	}
	if strings.Contains(raw, "/auth/v1/verify") || strings.Contains(raw, "type=reauthentication") {
		t.Errorf("reauthentication must not include a verify link:\n%s", raw)
	}
}

func TestSupabaseHookReauthenticationRequiresToken(t *testing.T) {
	fake := startFakeSMTP(t, "H00KREA2")
	cfg := testConfig(fake.addr())
	addTestHook(cfg, testHookSecret, "https://project.supabase.co", "noreply@example.com")
	srv := newTestServer(t, cfg)

	body := supabasePayload("reauthentication", "user@example.net", "", "reauth-hash")
	resp := postHook(t, srv, body, signWebhook(testHookSecret, "msg_reauth2", time.Now(), body))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", resp.StatusCode, readBody(t, resp))
	}
	if got := len(fake.messages()); got != 0 {
		t.Fatalf("queued %d messages without an OTP, want 0", got)
	}
}

func TestSupabaseNotificationContent(t *testing.T) {
	payload := supabaseHookPayload{}
	payload.User.Email = "user@example.net"
	payload.EmailData.OldEmail = "old@example.net"
	payload.EmailData.OldPhone = "+15551234567"
	payload.EmailData.Provider = "google"
	payload.EmailData.FactorType = "totp"

	cases := []struct {
		action   string
		subject  string
		contains string
	}{
		{"password_changed_notification", "Your password was changed", "password"},
		{"email_changed_notification", "Your email address was changed", "old@example.net"},
		{"phone_changed_notification", "Your phone number was changed", "+15551234567"},
		{"identity_linked_notification", "A sign-in method was linked", "google"},
		{"identity_unlinked_notification", "A sign-in method was removed", "google"},
		{"mfa_factor_enrolled_notification", "Two-factor authentication was enabled", "totp"},
		{"mfa_factor_unenrolled_notification", "Two-factor authentication was disabled", "totp"},
		{"some_future_notification", "Your account was updated", "security-related"},
	}

	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			subject, lines, ok := supabaseNotification(tc.action, payload)
			if !ok {
				t.Fatalf("%s was not recognized as a notification", tc.action)
			}
			if subject != tc.subject {
				t.Errorf("subject = %q, want %q", subject, tc.subject)
			}
			if joined := strings.ToLower(strings.Join(lines, " ")); !strings.Contains(joined, tc.contains) {
				t.Errorf("body = %q, want it to contain %q", joined, tc.contains)
			}
		})
	}

	if _, _, ok := supabaseNotification("signup", payload); ok {
		t.Error("signup must not be treated as a notification")
	}
}

func TestSupabaseHookSendsNotificationWithoutToken(t *testing.T) {
	fake := startFakeSMTP(t, "H00KNOTI")
	cfg := testConfig(fake.addr())
	addTestHook(cfg, testHookSecret, "https://project.supabase.co", "noreply@example.com")
	srv := newTestServer(t, cfg)

	body := supabasePayloadWithData("password_changed_notification", "user@example.net", nil)
	resp := postHook(t, srv, body, signWebhook(testHookSecret, "msg_notify", time.Now(), body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}

	messages := fake.messages()
	if len(messages) != 1 {
		t.Fatalf("queued %d messages, want 1", len(messages))
	}
	raw := string(messages[0])
	if !strings.Contains(raw, "Subject: Your password was changed") {
		t.Errorf("message has the wrong subject:\n%s", raw)
	}
	if strings.Contains(raw, "Or enter this code") || strings.Contains(raw, "/auth/v1/verify") {
		t.Errorf("a notification must not carry a link or code:\n%s", raw)
	}
}

func TestSupabaseHookDedupesWebhookID(t *testing.T) {
	fake := startFakeSMTP(t, "H00KDEDU")
	cfg := testConfig(fake.addr())
	addTestHook(cfg, testHookSecret, "https://project.supabase.co", "noreply@example.com")
	srv := newTestServer(t, cfg)

	body := supabasePayload("signup", "user@example.net", "305805", "abc123hash")
	headers := signWebhook(testHookSecret, "msg_once", time.Now(), body)

	first := postHook(t, srv, body, headers)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200: %s", first.StatusCode, readBody(t, first))
	}

	second := postHook(t, srv, body, headers)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200: %s", second.StatusCode, readBody(t, second))
	}
	if second.Header.Get("Idempotent-Replay") != "true" {
		t.Error("the retry did not report Idempotent-Replay: true")
	}

	if got := len(fake.messages()); got != 1 {
		t.Fatalf("queued %d messages, want 1 for a repeated webhook-id", got)
	}
}

func TestSupabaseHookDedupeIsScopedToProject(t *testing.T) {
	fake := startFakeSMTP(t, "H00KPROJ")
	cfg := testConfig(fake.addr())
	addTestHook(cfg, []byte("secret-one"), "https://one.supabase.co", "one@example.com")
	addTestHook(cfg, []byte("secret-two"), "https://two.supabase.co", "two@example.com")
	srv := newTestServer(t, cfg)

	body := supabasePayload("signup", "user@example.net", "305805", "abc123hash")
	first := postHook(t, srv, body, signWebhook([]byte("secret-one"), "msg_shared", time.Now(), body))
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200: %s", first.StatusCode, readBody(t, first))
	}

	second := postHook(t, srv, body, signWebhook([]byte("secret-two"), "msg_shared", time.Now(), body))
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200: %s", second.StatusCode, readBody(t, second))
	}

	if got := len(fake.messages()); got != 2 {
		t.Fatalf("queued %d messages, want 2: the same id from another project is a different event", got)
	}
}

func TestSupabaseHookDedupeReleasesFailedAttempt(t *testing.T) {
	fake := startFakeSMTP(t, "H00KFAIL")
	cfg := testConfig(fake.addr())
	addTestHook(cfg, testHookSecret, "https://project.supabase.co", "noreply@example.com")
	srv := newTestServer(t, cfg)

	bad := supabasePayload("signup", "", "305805", "abc123hash")
	first := postHook(t, srv, bad, signWebhook(testHookSecret, "msg_retry", time.Now(), bad))
	if first.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("first status = %d, want 422: %s", first.StatusCode, readBody(t, first))
	}

	good := supabasePayload("signup", "user@example.net", "305805", "abc123hash")
	second := postHook(t, srv, good, signWebhook(testHookSecret, "msg_retry", time.Now(), good))
	if second.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", second.StatusCode, readBody(t, second))
	}
	if got := len(fake.messages()); got != 1 {
		t.Fatalf("queued %d messages, want 1 after the failed attempt released the id", got)
	}
}

func TestSupabaseDedupeKeyIsOutOfClientNamespace(t *testing.T) {
	key := supabaseDedupeKey("test", "msg_1")
	if !strings.HasPrefix(key, "\x00") {
		t.Fatalf("dedupe key = %q, want a leading NUL so it cannot equal an Idempotency-Key", key)
	}
	if key == supabaseDedupeKey("test", "msg_2") {
		t.Fatal("different webhook ids produced the same key")
	}
	if key == supabaseDedupeKey("other", "msg_1") {
		t.Fatal("different projects produced the same key")
	}
}

func TestSupabaseBrandName(t *testing.T) {
	cases := []struct {
		name    string
		brand   string
		siteURL string
		want    string
	}{
		{"configured brand wins", "Makero", "https://app.example.com", "Makero"},
		{"apex domain", "", "https://makero.diy", "Makero"},
		{"subdomain", "", "https://app.makero.diy/welcome", "Makero"},
		{"localhost", "", "http://localhost:3000", ""},
		{"ip address", "", "https://127.0.0.1", ""},
		{"empty", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := supabaseHook{brand: tc.brand}
			if got := hook.brandName(tc.siteURL); got != tc.want {
				t.Errorf("brandName(%q) = %q, want %q", tc.siteURL, got, tc.want)
			}
		})
	}
}

func TestComposeEmail(t *testing.T) {
	t.Run("auth with link and code", func(t *testing.T) {
		text, bodyHTML := composeEmail(emailContent{
			brand:   "Makero",
			heading: "Confirm your email address",
			intro:   "Thanks for signing up.",
			action:  "Confirm email address",
			link:    "https://example.com/verify?token=abc&type=signup",
			code:    "123456",
			note:    "If you didn't request this, you can safely ignore this email.",
		})

		for _, want := range []string{
			"Makero",
			"Confirm your email address",
			"Confirm email address",
			"123456",
			"https://example.com/verify?token=abc&amp;type=signup",
		} {
			if !strings.Contains(bodyHTML, want) {
				t.Errorf("HTML is missing %q:\n%s", want, bodyHTML)
			}
		}
		if strings.Contains(bodyHTML, "token=abc&type=") {
			t.Error("the link in the HTML was not escaped")
		}
		for _, want := range []string{
			"https://example.com/verify?token=abc&type=signup",
			"123456",
			"Makero",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("plain text is missing %q:\n%s", want, text)
			}
		}
	})

	t.Run("code only", func(t *testing.T) {
		text, bodyHTML := composeEmail(emailContent{
			brand:   "Makero",
			heading: "Confirm it's you",
			intro:   "Confirm your identity to continue.",
			code:    "123456",
			note:    "If you didn't request this, you can safely ignore this email.",
		})
		if strings.Contains(bodyHTML, "href=") {
			t.Errorf("a code-only email must not contain a link:\n%s", bodyHTML)
		}
		if !strings.Contains(text, "Enter this code to continue:") {
			t.Errorf("plain text is missing the code label:\n%s", text)
		}
		if !strings.Contains(text, "123456") {
			t.Errorf("plain text is missing the code:\n%s", text)
		}
	})

	t.Run("notification", func(t *testing.T) {
		text, bodyHTML := composeEmail(emailContent{
			brand:   "Makero",
			heading: "Your password was changed",
			lines:   []string{"The password for your account was changed."},
			note:    "This is an automated security notice for your account.",
		})
		if !strings.Contains(bodyHTML, "Your password was changed") {
			t.Errorf("HTML is missing the heading:\n%s", bodyHTML)
		}
		if strings.Contains(bodyHTML, "href=") {
			t.Errorf("a notification must not contain a link:\n%s", bodyHTML)
		}
		if !strings.Contains(text, "The password for your account was changed.") {
			t.Errorf("plain text is missing the notice:\n%s", text)
		}
	})
}

func TestSupabaseHookUsesConfiguredBrand(t *testing.T) {
	fake := startFakeSMTP(t, "H00KBRND")
	cfg := testConfig(fake.addr())
	addTestHook(cfg, testHookSecret, "https://project.supabase.co", "noreply@example.com")
	cfg.supabaseHooks[0].brand = "Makero"
	srv := newTestServer(t, cfg)

	body := supabasePayload("signup", "user@example.net", "305805", "abc123hash")
	resp := postHook(t, srv, body, signWebhook(testHookSecret, "msg_brand", time.Now(), body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}

	raw := string(fake.messages()[0])
	if !strings.Contains(raw, "From: \"Makero\" <noreply@example.com>") {
		t.Errorf("message is missing the branded sender:\n%s", raw)
	}
	if !strings.Contains(raw, "Makero") {
		t.Errorf("message is missing the configured brand:\n%s", raw)
	}
	if strings.Contains(raw, "Example") {
		t.Errorf("message used the derived brand instead of the configured one:\n%s", raw)
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
