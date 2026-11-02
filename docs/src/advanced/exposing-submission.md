# Exposing submission to another network

By default the submission port is only useful from the host it runs on. When a managed service outside your network
has to send through it — Supabase Auth's SMTP settings, a CI job, an application on another VPS — the port becomes
internet-facing and has to be treated like one.

Three protections are built in, so nobody has to remember them:

- Submission **requires STARTTLS**. The `submission` service runs with `smtpd_tls_security_level = encrypt`, so a
  plaintext client is refused with `530 5.7.0 Must issue a STARTTLS command first`, and `smtpd_tls_auth_only = yes`
  means credentials are never accepted before the upgrade.
- An authenticated client **may only use a `From:` address at a domain jmail signs** — `JMAIL_ORIGIN` plus everything
  in `JMAIL_DOMAINS`. Anything else is rejected with `554 5.7.1 Sender address rejected: Access denied` (reported at
  `RCPT TO`, because `smtpd_delay_reject = yes`). The allowlist is a `regexp:` map rendered at startup, so adding a
  domain to `JMAIL_DOMAINS` also allows it on submission. `permit_mynetworks` is applied first, which keeps the HTTP
  API's loopback submission unaffected.
- Submission is **rate-limited per client address**: 10 concurrent connections, 30 connections and 60 messages per
  minute, 100 recipients per minute, with soft and hard error limits at 10 and 20. The limits live in `main.cf`, so a
  `postfix.d` override changes them without a rebuild. Clients in `JMAIL_MYNETWORKS` are exempt through
  `smtpd_client_event_limit_exceptions`, which is why the HTTP API's loopback submissions are not throttled.

The rest of this page is deployment work: firewall, certificate, credentials, brute-force protection.

## 1. Open the port

`587` inside the container is published on whatever host port you chose, for example `10587`. Open only that port.

firewalld (Fedora, RHEL):

```sh
sudo firewall-cmd --permanent --add-port=10587/tcp
sudo firewall-cmd --reload
sudo firewall-cmd --list-ports
```

If the sender is self-hosted and has a stable egress address, allow just that instead:

```sh
sudo firewall-cmd --permanent --add-rich-rule=\
  'rule family=ipv4 source address=198.51.100.10/32 port port=10587 protocol=tcp accept'
```

Supabase Cloud egresses from rotating addresses, so an allowlist is only practical for a self-hosted instance or a
fixed-egress proxy. Check the hosting provider's own network firewall too; on OVH it is a separate setting in the
manager.

Keep the DNS record for the submission hostname **DNS-only** (grey cloud). The Cloudflare proxy cannot carry SMTP.

## 2. Serve a certificate the client accepts

jmail generates a self-signed certificate on first boot. Go's SMTP client — what Supabase Auth uses — verifies both
the chain and the hostname, and there is no setting to skip it, so a self-signed certificate fails with
`x509: certificate signed by unknown authority` or `certificate is valid for ...`.

Use a CA certificate. DNS-01 avoids needing inbound port 80:

```sh
sudo dnf install -y certbot python3-certbot-dns-cloudflare
sudo certbot certonly --dns-cloudflare \
  --dns-cloudflare-credentials /root/.secrets/cloudflare.ini \
  -d mail.example.com
```

Install it into the volume jmail already reads. With rootless podman the container's root is your user, so the files
must be owned by that user (the generated pair is `server.crt` 0644, `server.key` 0600):

```sh
sudo install -m 600 -o "$USER" -g "$USER" \
  /etc/letsencrypt/live/mail.example.com/privkey.pem  "$HOME/.local/share/jmail/postfix-ssl/server.key"
sudo install -m 644 -o "$USER" -g "$USER" \
  /etc/letsencrypt/live/mail.example.com/fullchain.pem "$HOME/.local/share/jmail/postfix-ssl/server.crt"
systemctl --user restart jmail
```

jmail only generates its own pair when those files are missing, so yours is left alone. Renew with a deploy hook:

```sh
# /etc/letsencrypt/renewal-hooks/deploy/jmail.sh
#!/bin/sh
set -eu
dest=/home/fedora/.local/share/jmail/postfix-ssl
install -m 600 -o fedora -g fedora /etc/letsencrypt/live/mail.example.com/privkey.pem  "$dest/server.key"
install -m 644 -o fedora -g fedora /etc/letsencrypt/live/mail.example.com/fullchain.pem "$dest/server.crt"
sudo -u fedora XDG_RUNTIME_DIR=/run/user/1000 systemctl --user restart jmail
```

The certificate must name the host the client connects to. `JMAIL_HOSTNAME` is the **outbound** identity — the SMTP
banner and the PTR record — and does not have to be the same name.

## 3. Use a credential you can revoke on its own

The image creates one SASL account, `JMAIL_SMTP_USER`. Give the external service a second one so revoking it does not
touch your own tests. `/etc/jmail/init.d` runs as root before Postfix starts, and hook files can be bind-mounted from
`$HOME/.local/share/jmail/init.d`:

```sh
# $HOME/.local/share/jmail/init.d/10-supabase-user.sh
#!/bin/sh
set -eu
[ -n "${SUPABASE_SMTP_PASSWORD:-}" ] || exit 0
printf '%s' "$SUPABASE_SMTP_PASSWORD" | saslpasswd2 -p -c -u "$JMAIL_ORIGIN" supabase
```

```sh
chmod 0755 "$HOME/.local/share/jmail/init.d/10-supabase-user.sh"
```

Add the password to `$HOME/.config/jmail/jmail.env` (`openssl rand -hex 32`); the installer preserves keys it does
not manage:

```
SUPABASE_SMTP_PASSWORD=<generated>
```

Use `supabase` as the SMTP username. Remove the variable (or the hook) and restart to revoke it. Like
`JMAIL_SMTP_PASSWORD`, the value is visible to anyone who can run `podman inspect`, so treat it as a secret with the
same care.

## 4. Ban brute force

The stock `postfix` fail2ban filter does not match this image's log lines: Postfix writes its own timestamped prefix
and the service is `postfix/submissions`. Use a filter that matches what the container actually logs:

```ini
# /etc/fail2ban/filter.d/jmail-sasl.conf
[Definition]
failregex = ^.*postfix/(?:submissions?/)?smtpd\[\d+\]: warning: [^]]*\[<HOST>\]\S*: SASL \w+ authentication failed:.*$
ignoreregex =
```

```ini
# /etc/fail2ban/jail.d/jmail.conf
[jmail-sasl]
enabled      = true
filter       = jmail-sasl
backend      = systemd
journalmatch = CONTAINER_NAME=jmail
maxretry     = 8
findtime     = 10m
bantime      = 1h
ignoreip     = 127.0.0.1/8 ::1 <the server's own public IP>
```

```sh
sudo systemctl restart fail2ban
sudo fail2ban-client status jmail-sasl
```

`CONTAINER_NAME=jmail` is the journal field to match. Include the server's own public IP in `ignoreip`: submissions
made on the host are logged with that address, so a mistyped password in a local test would otherwise ban you.

## 5. Check it from outside

```sh
# The certificate must verify for the name the client uses.
openssl s_client -connect mail.example.com:10587 \
  -starttls smtp -servername mail.example.com </dev/null | head -1

# A real message, with the same settings the external service will use.
./scripts/send-test-mail.sh --server mail.example.com --port 10587 \
  --from noreply@example.com --to you@example.com
```

Then confirm the delivered headers show `DKIM-Signature` with your domain and `Authentication-Results` passing SPF and
DKIM at the receiver. `./scripts/check-dns.sh` verifies the records that decide those results.

## If the port does not have to be public

- **Self-hosted sender**: put both ends on WireGuard or Tailscale and publish the port only on the tunnel address, for
  example `PublishPort=100.x.y.z:10587:587` in the quadlet. Nothing listens publicly.
- **Application mail rather than sign-up mail**: the optional HTTP API authenticates each request with a bearer token
  and is easier to put behind a TLS reverse proxy than SMTP with a reusable password. Supabase Auth still needs SMTP,
  but application notifications can use `/v1/send`; see [HTTP API](../guide/http-api.md).

## Things not to do

- Do not publish port `25`, and do not widen `JMAIL_MYNETWORKS`. Either one turns jmail into an open relay.
- Do not reuse `JMAIL_SMTP_PASSWORD` for the external service.
- Do not put the submission hostname behind the Cloudflare proxy.
- Do not assume the self-signed certificate is enough for a managed sender; test the TLS handshake first.
