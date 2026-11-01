package main

import (
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
