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
		if cfg.supabaseHookSecret != nil {
			t.Fatal("the hook should be disabled when no secret is set")
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
		if got, want := string(cfg.supabaseHookSecret), "hook-secret-bytes"; got != want {
			t.Errorf("secret = %q, want %q", got, want)
		}
		if got, want := cfg.supabaseURL, "https://project.supabase.co"; got != want {
			t.Errorf("url = %q, want %q (trailing slash trimmed)", got, want)
		}
		if got, want := cfg.supabaseFrom, "noreply@example.com"; got != want {
			t.Errorf("from = %q, want %q", got, want)
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
		if cfg.supabaseHookSecret == nil {
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
