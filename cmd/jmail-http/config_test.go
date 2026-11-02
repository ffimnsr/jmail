package main

import (
	"encoding/base64"
	"fmt"
	"testing"
)

// The signing tables and the sender policy are rendered from the same list, so
// normalization has to be predictable: primary first, deduplicated, lowercased.
func TestParseDomainsPutsThePrimaryFirst(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"unset", "", "origin.example"},
		{"comma separated", "a.example,b.example", "origin.example a.example b.example"},
		{"space separated", "a.example b.example", "origin.example a.example b.example"},
		{"normalized", " A.Example. , @b.example ", "origin.example a.example b.example"},
		{"primary repeated", "origin.example", "origin.example"},
		{"duplicate", "a.example, a.example", "origin.example a.example"},
		{"junk dropped", "bad_domain, , a.example", "origin.example a.example"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fmt.Sprint(parseDomains(tc.raw, "origin.example"))
			if want := "[" + tc.want + "]"; got != want {
				t.Fatalf("parseDomains(%q) = %s, want %s", tc.raw, got, want)
			}
		})
	}
}

// A domain that is signed but not allowed by the API would be useless, and the
// reverse would send mail out unsigned, so the default policy follows the list.
func TestAllowedFromDefaultsToEverySigningDomain(t *testing.T) {
	t.Setenv("JMAIL_HTTP_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("JMAIL_ORIGIN", "example.com")
	t.Setenv("JMAIL_DOMAINS", "second.example")
	t.Setenv("JMAIL_HTTP_ALLOWED_FROM", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if want := "[example.com second.example]"; fmt.Sprint(cfg.allowedDomains()) != want {
		t.Errorf("allowedDomains() = %v, want %s", cfg.allowedDomains(), want)
	}
	for _, allowed := range []string{"sender@example.com", "sender@second.example"} {
		if !cfg.fromAllowed(allowed) {
			t.Errorf("%s should be allowed", allowed)
		}
	}
	if cfg.fromAllowed("sender@third.example") {
		t.Error("sender@third.example is not signed, so it should be rejected")
	}
}

// The hook is off unless a secret is configured, and a misconfigured sender is a
// startup error rather than a surprise on the first signup.
func TestSupabaseHookConfig(t *testing.T) {
	t.Run("disabled without a secret", func(t *testing.T) {
		t.Setenv("JMAIL_HTTP_TOKEN", "0123456789abcdef0123456789abcdef")
		t.Setenv("JMAIL_ORIGIN", "example.com")
		t.Setenv("JMAIL_HTTP_SUPABASE_HOOK_SECRET", "")

		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if len(cfg.supabaseHooks) != 0 {
			t.Fatalf("configured %d hooks, want 0 when no secret is set", len(cfg.supabaseHooks))
		}
	})

	t.Run("secret and defaults", func(t *testing.T) {
		secret := base64.StdEncoding.EncodeToString([]byte("hook-secret-bytes"))
		t.Setenv("JMAIL_HTTP_TOKEN", "0123456789abcdef0123456789abcdef")
		t.Setenv("JMAIL_ORIGIN", "example.com")
		t.Setenv("JMAIL_HTTP_SUPABASE_HOOK_SECRET", "v1,whsec_"+secret)
		t.Setenv("JMAIL_HTTP_SUPABASE_URL", "https://project.supabase.co/")
		t.Setenv("JMAIL_HTTP_SUPABASE_FROM", "")

		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if len(cfg.supabaseHooks) != 1 {
			t.Fatalf("configured %d hooks, want 1", len(cfg.supabaseHooks))
		}
		hook := cfg.supabaseHooks[0]
		if got, want := string(hook.secret), "hook-secret-bytes"; got != want {
			t.Errorf("secret = %q, want %q", got, want)
		}
		if got, want := hook.apiURL, "https://project.supabase.co"; got != want {
			t.Errorf("url = %q, want %q (trailing slash trimmed)", got, want)
		}
		if got, want := hook.from, "noreply@example.com"; got != want {
			t.Errorf("from = %q, want %q", got, want)
		}
		if got, want := hook.name, "JMAIL_HTTP_SUPABASE"; got != want {
			t.Errorf("name = %q, want %q", got, want)
		}
	})

	t.Run("hook secret works without a bearer token", func(t *testing.T) {
		secret := base64.StdEncoding.EncodeToString([]byte("hook-secret-bytes"))
		t.Setenv("JMAIL_HTTP_TOKEN", "")
		t.Setenv("JMAIL_ORIGIN", "example.com")
		t.Setenv("JMAIL_HTTP_SUPABASE_HOOK_SECRET", "v1,whsec_"+secret)

		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("a hook-only deployment should start: %v", err)
		}
		if cfg.token != "" {
			t.Errorf("token = %q, want empty", cfg.token)
		}
		if len(cfg.supabaseHooks) != 1 {
			t.Fatal("the hook should be enabled")
		}
	})

	t.Run("neither a token nor a hook secret", func(t *testing.T) {
		t.Setenv("JMAIL_HTTP_TOKEN", "")
		t.Setenv("JMAIL_ORIGIN", "example.com")
		t.Setenv("JMAIL_HTTP_SUPABASE_HOOK_SECRET", "")

		if _, err := loadConfig(); err == nil {
			t.Fatal("a service with nothing to serve should refuse to start")
		}
	})

	t.Run("several projects", func(t *testing.T) {
		first := base64.StdEncoding.EncodeToString([]byte("secret-one"))
		second := base64.StdEncoding.EncodeToString([]byte("secret-two"))
		t.Setenv("JMAIL_HTTP_TOKEN", "")
		t.Setenv("JMAIL_ORIGIN", "example.com")
		t.Setenv("JMAIL_HTTP_SUPABASE_HOOK_SECRET", "")
		t.Setenv("JMAIL_HTTP_SUPABASE_1_HOOK_SECRET", "v1,whsec_"+first)
		t.Setenv("JMAIL_HTTP_SUPABASE_1_URL", "https://one.supabase.co")
		t.Setenv("JMAIL_HTTP_SUPABASE_1_FROM", "one@example.com")
		t.Setenv("JMAIL_HTTP_SUPABASE_2_HOOK_SECRET", "v1,whsec_"+second)
		t.Setenv("JMAIL_HTTP_SUPABASE_2_URL", "https://two.supabase.co")

		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if len(cfg.supabaseHooks) != 2 {
			t.Fatalf("configured %d hooks, want 2", len(cfg.supabaseHooks))
		}
		if got, want := cfg.supabaseHooks[0].from, "one@example.com"; got != want {
			t.Errorf("first from = %q, want %q", got, want)
		}
		if got, want := cfg.supabaseHooks[1].from, "noreply@example.com"; got != want {
			t.Errorf("second from = %q, want the JMAIL_ORIGIN default %q", got, want)
		}
		if got, want := cfg.supabaseHooks[1].name, "JMAIL_HTTP_SUPABASE_2"; got != want {
			t.Errorf("second name = %q, want %q", got, want)
		}
	})

	t.Run("a broken second project fails at startup", func(t *testing.T) {
		first := base64.StdEncoding.EncodeToString([]byte("secret-one"))
		t.Setenv("JMAIL_HTTP_TOKEN", "")
		t.Setenv("JMAIL_ORIGIN", "example.com")
		t.Setenv("JMAIL_HTTP_SUPABASE_HOOK_SECRET", "")
		t.Setenv("JMAIL_HTTP_SUPABASE_1_HOOK_SECRET", "v1,whsec_"+first)
		t.Setenv("JMAIL_HTTP_SUPABASE_2_HOOK_SECRET", "not base64!")

		if _, err := loadConfig(); err == nil {
			t.Fatal("a bad secret in any project should stop startup")
		}
	})

	t.Run("rejects an unsigned sender", func(t *testing.T) {
		secret := base64.StdEncoding.EncodeToString([]byte("hook-secret-bytes"))
		t.Setenv("JMAIL_HTTP_TOKEN", "0123456789abcdef0123456789abcdef")
		t.Setenv("JMAIL_ORIGIN", "example.com")
		t.Setenv("JMAIL_HTTP_SUPABASE_HOOK_SECRET", "v1,whsec_"+secret)
		t.Setenv("JMAIL_HTTP_SUPABASE_FROM", "noreply@elsewhere.example")

		if _, err := loadConfig(); err == nil {
			t.Fatal("a sender outside the signing domains should fail at startup")
		}
	})

	t.Run("rejects a non-base64 secret", func(t *testing.T) {
		t.Setenv("JMAIL_HTTP_TOKEN", "0123456789abcdef0123456789abcdef")
		t.Setenv("JMAIL_ORIGIN", "example.com")
		t.Setenv("JMAIL_HTTP_SUPABASE_HOOK_SECRET", "not base64!")

		if _, err := loadConfig(); err == nil {
			t.Fatal("an unparseable secret should fail at startup")
		}
	})
}
