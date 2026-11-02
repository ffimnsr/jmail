package main

import (
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// config is read once at startup. Everything the service does is bounded by it,
// so a missing or nonsensical value is a startup error rather than a surprise
// under load.
type config struct {
	listen string
	token  string

	// origin is JMAIL_ORIGIN, the primary signing domain. domains is the full
	// JMAIL_DOMAINS list with origin first: OpenDKIM signs exactly these, so it
	// doubles as the default sender policy. hostname is JMAIL_HOSTNAME, used for
	// the EHLO name and the Message-ID domain.
	origin   string
	domains  []string
	hostname string

	smtpAddr    string
	smtpTimeout time.Duration
	concurrency int

	// drain bounds how long a SIGTERM waits for in-flight submissions. It has to
	// stay under s6-overlay's S6_KILL_GRACETIME (3000 ms by default) and under
	// the runtime's stop timeout, or the process is SIGKILLed before it finishes.
	drain time.Duration

	allowedFrom  []string
	allowAnyFrom bool

	// supabaseHooks enables the Supabase Auth Send Email Hook route when it is
	// non-empty. Each entry is one Supabase project: its own signing secret, its
	// own project API URL for confirmation links, and its own sender. A request
	// is matched to the project whose secret verifies the signature.
	supabaseHooks []supabaseHook

	maxBytes       int64
	maxRecipients  int
	maxAttachments int

	rate  float64
	burst int

	// logRate caps access-log lines per second; 0 disables sampling.
	logRate float64

	// idempotencyDir holds the replay file. Empty disables persistence.
	idempotencyDir string

	idempotencyTTL time.Duration
}

func loadConfig() (*config, error) {
	cfg := &config{
		listen:   envString("JMAIL_HTTP_LISTEN", "0.0.0.0:8080"),
		token:    strings.TrimSpace(os.Getenv("JMAIL_HTTP_TOKEN")),
		origin:   strings.ToLower(strings.TrimSpace(envString("JMAIL_ORIGIN", "example.com"))),
		hostname: envString("JMAIL_HOSTNAME", "localhost"),
		smtpAddr: envString("JMAIL_HTTP_SMTP_ADDR", "127.0.0.1:25"),
	}

	// Fail closed: an unauthenticated relay is an open relay. The check for
	// "nothing to serve at all" happens after the hook secrets are read, since any
	// Supabase project also keeps the service running.
	if cfg.origin == "" {
		return nil, fmt.Errorf("JMAIL_ORIGIN must be set; it defines the default allowed sending domain")
	}
	cfg.domains = parseDomains(envString("JMAIL_DOMAINS", ""), cfg.origin)

	var err error
	if cfg.smtpTimeout, err = envDuration("JMAIL_HTTP_TIMEOUT", 30*time.Second); err != nil {
		return nil, err
	}
	if cfg.drain, err = envDuration("JMAIL_HTTP_DRAIN", 2*time.Second); err != nil {
		return nil, err
	}
	if cfg.concurrency, err = envInt("JMAIL_HTTP_CONCURRENCY", 16); err != nil {
		return nil, err
	}
	if cfg.maxRecipients, err = envInt("JMAIL_HTTP_MAX_RECIPIENTS", 50); err != nil {
		return nil, err
	}
	if cfg.maxAttachments, err = envInt("JMAIL_HTTP_MAX_ATTACHMENTS", 10); err != nil {
		return nil, err
	}
	if cfg.burst, err = envInt("JMAIL_HTTP_RATE_BURST", 20); err != nil {
		return nil, err
	}
	if cfg.maxBytes, err = envBytes("JMAIL_HTTP_MAX_BYTES", 5<<20); err != nil {
		return nil, err
	}
	if cfg.rate, err = envFloat("JMAIL_HTTP_RATE", 10); err != nil {
		return nil, err
	}
	if cfg.logRate, err = envFloatAllowZero("JMAIL_HTTP_LOG_RATE", 10); err != nil {
		return nil, err
	}
	if cfg.idempotencyTTL, err = envDuration("JMAIL_HTTP_IDEMPOTENCY_TTL", time.Hour); err != nil {
		return nil, err
	}

	cfg.idempotencyDir = envString("JMAIL_HTTP_IDEMPOTENCY_DIR", "/var/lib/jmail-http")
	if strings.EqualFold(cfg.idempotencyDir, "off") || strings.EqualFold(cfg.idempotencyDir, "none") {
		cfg.idempotencyDir = ""
	}

	cfg.allowedFrom, cfg.allowAnyFrom = parseAllowedFrom(envString("JMAIL_HTTP_ALLOWED_FROM", ""), cfg.domains)

	// The Supabase hook is only routed when at least one secret is configured, so
	// an installation that does not use Supabase Auth exposes nothing extra.
	// Unnumbered variables describe one project; JMAIL_HTTP_SUPABASE_<N>_* adds
	// more, because each Supabase project generates its own hook secret.
	if raw := strings.TrimSpace(os.Getenv("JMAIL_HTTP_SUPABASE_HOOK_SECRET")); raw != "" {
		hook, err := loadSupabaseHook(cfg, "JMAIL_HTTP_SUPABASE", raw,
			envString("JMAIL_HTTP_SUPABASE_URL", ""),
			envString("JMAIL_HTTP_SUPABASE_FROM", ""))
		if err != nil {
			return nil, err
		}
		cfg.supabaseHooks = append(cfg.supabaseHooks, hook)
	}

	for i := 1; i <= maxSupabaseHooks; i++ {
		prefix := fmt.Sprintf("JMAIL_HTTP_SUPABASE_%d", i)
		raw := strings.TrimSpace(os.Getenv(prefix + "_HOOK_SECRET"))
		if raw == "" {
			continue
		}
		hook, err := loadSupabaseHook(cfg, prefix, raw,
			envString(prefix+"_URL", ""),
			envString(prefix+"_FROM", ""))
		if err != nil {
			return nil, err
		}
		cfg.supabaseHooks = append(cfg.supabaseHooks, hook)
	}

	// A token is not required for hook-only deployments, because that route
	// authenticates each request with its own signature, but a service with
	// neither has nothing to serve.
	if cfg.token == "" && len(cfg.supabaseHooks) == 0 {
		return nil, fmt.Errorf("JMAIL_HTTP_TOKEN or at least one JMAIL_HTTP_SUPABASE_*_HOOK_SECRET must be set; the API refuses to start with neither")
	}

	return cfg, nil
}

// maxSupabaseHooks bounds how many Supabase projects one endpoint accepts.
// Verifying a request costs one HMAC per configured project, so this is a
// sanity limit rather than a performance one.
const maxSupabaseHooks = 16

// loadSupabaseHook validates one project's settings. The empty sender falls back
// to noreply@JMAIL_ORIGIN, the same default a single-project deployment gets.
func loadSupabaseHook(cfg *config, envPrefix, rawSecret, rawURL, rawFrom string) (supabaseHook, error) {
	secret, err := parseWebhookSecret(rawSecret)
	if err != nil {
		return supabaseHook{}, fmt.Errorf("%s_HOOK_SECRET: %w", envPrefix, err)
	}

	apiURL := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if apiURL != "" {
		parsed, err := url.Parse(apiURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return supabaseHook{}, fmt.Errorf("%s_URL must be an http(s) URL, got %q", envPrefix, apiURL)
		}
	}

	from := strings.TrimSpace(rawFrom)
	if from == "" {
		from = "noreply@" + cfg.origin
	}
	if _, err := mail.ParseAddress(from); err != nil {
		return supabaseHook{}, fmt.Errorf("%s_FROM: %w", envPrefix, err)
	}
	if !cfg.fromAllowed(from) {
		return supabaseHook{}, fmt.Errorf("%s_FROM %q is outside the allowed sender domains %v",
			envPrefix, from, cfg.allowedDomains())
	}

	return supabaseHook{name: envPrefix, secret: secret, apiURL: apiURL, from: from}, nil
}

// allowedDomains is the list used in error messages, so a rejected sender tells
// the caller what would have worked.
func (c *config) allowedDomains() []string {
	if c.allowAnyFrom {
		return []string{"*"}
	}
	return c.allowedFrom
}

// fromAllowed enforces a match against the configured domains rather than a
// subdomain wildcard on purpose: the OpenDKIM signing table covers exactly
// *@<domain> for each entry in JMAIL_DOMAINS with SubDomains no, so anything
// else leaves the message unsigned.
func (c *config) fromAllowed(addr string) bool {
	if c.allowAnyFrom {
		return true
	}
	domain := addressDomain(addr)
	for _, allowed := range c.allowedFrom {
		if domain == allowed {
			return true
		}
	}
	return false
}

func addressDomain(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(addr[at+1:]), ".")
}

func parseAllowedFrom(raw string, defaults []string) (domains []string, allowAny bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return append([]string(nil), defaults...), false
	}

	seen := make(map[string]bool)
	for _, field := range splitDomainList(raw) {
		if field == "*" {
			return nil, true
		}
		if seen[field] {
			continue
		}
		seen[field] = true
		domains = append(domains, field)
	}
	if len(domains) == 0 {
		return append([]string(nil), defaults...), false
	}
	return domains, false
}

// parseDomains normalizes JMAIL_DOMAINS and always puts the primary domain
// first, so the signing table and the default sender policy agree. The init
// script is the authority on rejecting bad values; here an implausible name is
// dropped rather than allowed through to the signing tables.
func parseDomains(raw, origin string) []string {
	domains := []string{origin}
	seen := map[string]bool{origin: true}
	for _, domain := range splitDomainList(raw) {
		if seen[domain] || !isDomainName(domain) {
			continue
		}
		seen[domain] = true
		domains = append(domains, domain)
	}
	return domains
}

// splitDomainList splits on commas and whitespace and normalizes each entry the
// way the init script does, so "a.example,b.example" and "a.example b.example"
// are both accepted.
func splitDomainList(raw string) []string {
	var fields []string
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		field = strings.ToLower(strings.TrimSpace(field))
		field = strings.TrimPrefix(field, "@")
		field = strings.TrimSuffix(field, ".")
		if field == "" {
			continue
		}
		fields = append(fields, field)
	}
	return fields
}

// isDomainName mirrors the check the init script applies to JMAIL_DOMAINS, so a
// standalone run of this binary cannot accept a domain Postfix would refuse.
func isDomainName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero, got %d", key, n)
	}
	return n, nil
}

func envFloat(key string, def float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if f <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero, got %v", key, f)
	}
	return f, nil
}

// envFloatAllowZero is envFloat for settings where zero turns a feature off.
func envFloatAllowZero(key string, def float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %v", key, f)
	}
	return f, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero, got %s", key, d)
	}
	return d, nil
}

// envBytes accepts a plain byte count or a K/M/G suffix, optionally followed by
// B, so both 5242880 and 5MB work.
func envBytes(key string, def int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}

	value := strings.ToUpper(strings.TrimSuffix(raw, "B"))
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(value, "K"):
		multiplier, value = 1<<10, strings.TrimSuffix(value, "K")
	case strings.HasSuffix(value, "M"):
		multiplier, value = 1<<20, strings.TrimSuffix(value, "M")
	case strings.HasSuffix(value, "G"):
		multiplier, value = 1<<30, strings.TrimSuffix(value, "G")
	}

	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero, got %s", key, raw)
	}
	return n * multiplier, nil
}
