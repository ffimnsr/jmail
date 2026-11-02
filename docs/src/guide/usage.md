# Usage

## SMTP settings

Use these settings in your application, mail client or another mail server:

| Setting | Value |
| --- | --- |
| Host | Your `JMAIL_HOSTNAME`, or the container host |
| Port | `587` (submission) |
| Encryption | STARTTLS |
| Username | `JMAIL_SMTP_USER` |
| Password | `JMAIL_SMTP_PASSWORD` |

The account is created in the SASL database under the `JMAIL_ORIGIN` realm, so `JMAIL_SMTP_USER` and
`user@JMAIL_ORIGIN` both authenticate. Authentication is only offered after STARTTLS
(`smtpd_tls_auth_only = yes`), and the `submission` service requires TLS outright, so a plaintext client is refused
with `530 5.7.0 Must issue a STARTTLS command first`. One account covers every domain in `JMAIL_DOMAINS`: domains are
signing identities, not users, and there is no per-domain credential.

An authenticated client may only use a `From:` address at `JMAIL_ORIGIN` or one of `JMAIL_DOMAINS`; anything else is
rejected at `RCPT TO` with `554 5.7.1 Sender address rejected`. That keeps a leaked credential from being usable to
send as an arbitrary domain. Before opening the port to another network, read
[Exposing submission](../advanced/exposing-submission.md).

Relaying is only permitted for authenticated clients and for the networks listed in `JMAIL_MYNETWORKS`. It defaults to
`127.0.0.0/8`, which means only the container itself; everything else has to authenticate.

That default is deliberate. The image previously trusted all of RFC 1918 (`10.0.0.0/8`, `172.16.0.0/12`,
`192.168.0.0/16`), which quietly turned every container or VM sharing one of those ranges into an unauthenticated
relay — with mail signed by your DKIM key. If you really need unauthenticated relay from a specific network, widen it
explicitly:

```sh
-e JMAIL_MYNETWORKS="10.0.0.0/8 127.0.0.0/8"
```

## Sending a test message

The image renders a helper script at `/usr/local/bin/test-mail.sh` that mails `JMAIL_ROOT_EMAIL`:

```sh
podman exec -it jmail test-mail.sh
podman logs --tail 50 jmail
```

The scripts in the repository root, `test-auth.py` and `test-auth-send-mail.py`, connect over STARTTLS and
authenticate; the second one also sends a message. They read `JMAIL_TEST_SERVER`, `JMAIL_TEST_PORT`,
`JMAIL_TEST_USER`, `JMAIL_TEST_PASSWORD`, and for the second script `JMAIL_TEST_SENDER` and `JMAIL_TEST_RECEIVER`,
falling back to the placeholder values in the file. `mk test-auth` and `mk test-send` run them against the local dev
container with those variables already set.

`scripts/send-test-mail.sh` is the interactive shortcut. It asks for the server, port, credentials, From and To
addresses, subject and body (a random quote by default), prints a summary, sends over STARTTLS and reports what the
server accepted. On the machine where jmail is installed it pre-fills everything from `jmail.env` and connects to
`127.0.0.1`, because the public hostname usually needs NAT hairpinning from the host itself. It works without a clone:

```sh
curl -fsSL https://raw.githubusercontent.com/ffimnsr/jmail/master/scripts/send-test-mail.sh | bash
```

For scripted use, pass `--yes` with `--to`, `--from`, `--subject` and the rest; `--dry-run` prints the message without
connecting. `mk send-test-mail` runs it from the repository, and `./scripts/check-dns.sh` verifies the records that
decide whether the message authenticates.

`test-http.sh` does the same over the optional HTTP API, using curl: it checks health, a missing token, malformed
JSON, the sender policy, a real send, and an idempotent replay, and exits non-zero on the first failure. It reads
`JMAIL_TEST_HTTP_SERVER`, `JMAIL_TEST_HTTP_PORT` and `JMAIL_TEST_HTTP_TOKEN`. `mk test-http` runs it against the dev
container; see [HTTP API](http-api.md).

## TLS

On first boot jmail generates a self-signed ECDSA (P-384) certificate valid for ten years, with `JMAIL_ORIGIN`
as its common name and a subject alternative name for every domain in `JMAIL_DOMAINS`. The certificate and key live in
`/etc/postfix/ssl/`, which is the volume to persist (mounting all of `/etc/postfix` also works, since that is a volume
in the image as well).

To use a real certificate, put your own `server.crt` and `server.key` in that directory and restart the container.
jmail installs its generated certificate only when `server.crt` or `server.key` is missing, so your own files are
left alone. (The log still prints the self-signed certificate it generates at startup, because it does not know yet
whether one of the two files is going to be supplied later.)

To regenerate the self-signed certificate (for example after changing `JMAIL_ORIGIN` or `JMAIL_DOMAINS`), create the
trigger file and restart:

```sh
touch /etc/postfix/ssl/.generate   # inside the /etc/postfix/ssl volume
podman restart jmail
```

## DKIM

jmail signs every domain listed in `JMAIL_DOMAINS`, which always includes `JMAIL_ORIGIN` as the primary domain. A
2048-bit RSA key is generated per domain on first boot and stored in `/etc/opendkim/keys/<domain>/sendonly.private`,
so adding a domain creates one new key directory and leaves the existing keys untouched. The matching DNS records are
printed to the log, each labelled with the name it belongs under, and are also written to `sendonly.txt` next to the
key.

Every message sent through jmail is signed as `sendonly._domainkey.<domain>`, where the domain is the one in `From:`.
The signing table maps `*@<domain>` for each configured domain, with `SubDomains no`, so a message whose `From:`
address is at a domain outside the list (or at a subdomain of one) is passed through **unsigned** rather than rejected.
Set `JMAIL_DOMAINS` to sign more domains, or extend the signing and key tables by hand for anything the list cannot
express — see [Customization](../advanced/customization.md).

A single key per domain is deliberate: the selector and the key belong to the domain, so merging domains onto one key
would make a compromise impossible to scope and a rotation impossible to stage per domain.

OpenDKIM signs the RFC 6376 recommended header set, and the template widens it through the `SignHeaders` directive
with `Message-ID`, `MIME-Version`, `Content-Type`, `Content-Transfer-Encoding`, `List-Unsubscribe-Post` and
`Feedback-ID`. Only headers present in the message are hashed, so a multipart message has no top-level
`Content-Transfer-Encoding` to cover. The tradeoff of covering the MIME headers is that anything which legitimately
rewrites `Content-Type` in transit, for example a re-encoding mailing-list gateway, will also break the signature. The
`h=` tag in the signature is the authoritative list of what was covered, and `postcat -q <queue-id>` inside the
container shows it with the message.

To rotate the key for one domain, create the trigger file in that domain's directory and restart:

```sh
mkdir -p /etc/opendkim/keys/other.example               # inside the /etc/opendkim/keys volume
touch /etc/opendkim/keys/other.example/.generate
podman restart jmail
```

A new key replaces the old one, so update that domain's DKIM record in DNS before the next message goes out. Rotating
one domain does not touch the keys of the others.

### Publishing a 2048-bit key

A 2048-bit public key produces a TXT record of roughly 400 characters. DNS stores TXT records as a list of
255-character strings, so the record has to be split into quoted chunks on a single line:

```
sendonly._domainkey IN TXT ( "v=DKIM1; k=rsa; "
  "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA..."
  "...remaining base64..." )
```

The `sendonly.txt` file next to the key and the log output are already printed in this split form, so copy them
verbatim. Most DNS providers and APIs accept the quoted chunks and rejoin them; a few only accept a single
unquoted string and reject records over 255 characters. If yours does, the clean fix is to move the record to a
provider that can store long TXT values. Falling back to a 1024-bit key means changing the two `opendkim-genkey`
calls in `/etc/s6-overlay/s6-rc.d/init-mailer-config/run` and rebuilding the image, and 1024-bit RSA keys are
increasingly treated as weak by receivers.

## Where the configuration lives

jmail renders Postfix, OpenDKIM, SASL and syslog configuration from the templates in `/etc/templates` using
[dockerize](https://github.com/jwilder/dockerize). The rendered files are regenerated from the templates on every
start, so:

- Changing an environment variable requires recreating the container, not just restarting it.
- Editing the rendered files under `/etc/postfix` is overwritten on the next start.
- Changing a template requires rebuilding the image.

To change a Postfix setting the templates do not expose, or to run something at startup, use the override and hook
directories instead of editing rendered files: see [Customization](../advanced/customization.md).

## DNS checklist

Before sending production mail, work through this list. Each item is checked from the receiving side, so a working
local test send does not prove any of them.

- [ ] `A` record for `JMAIL_HOSTNAME` pointing at the server's public IP.
- [ ] `PTR` (rDNS) record on that IP resolving back to the same name. It is set at your hosting provider, and
      receivers compare it with the banner hostname.
- [ ] **No `MX` record is required.** jmail is outbound-only and does not accept inbound mail, so an MX record would
      advertise a service the container does not run. A pre-existing null MX (`v=MX:0 .`) is fine.
- [ ] `DKIM` record at `sendonly._domainkey.<domain>`, once per domain in `JMAIL_DOMAINS`, from the log or that
domain's `sendonly.txt`.
- [ ] `SPF` record on the domain you send `From:`, once per domain in `JMAIL_DOMAINS`. The example is a hard fail, not
      a soft fail:
  ```
  v=spf1 a:<mail-server-hostname> ip4:<server-ip> -all
  ```
- [ ] Outbound port `25` is open. Many providers block it, and mail then queues instead of being delivered.
- [ ] `DMARC` record. Start with `p=none` so you can read the reports, then tighten to reject on the domain and on
  subdomains:
  ```
  v=DMARC1; p=reject; sp=reject; rua=mailto:catchall@<mail-server-hostname>
  ```
- [ ] Check whether your IP is on an email blocklist (https://multirbl.valli.org), and delist it if it is.
- [ ] Send a test message to https://www.mail-tester.com and check the report.

`./scripts/check-dns.sh` covers the machine-checkable half of this list. It reads `~/.config/jmail/jmail.env`, so on
the mail server it needs no arguments, and it queries the live zone rather than the values you intended to publish:
the `A` record and forward-confirmed `PTR`, whether `SPF` authorizes the sending IP (following `include:` and
`redirect=`), whether the `DKIM` key is published and matches the one in the container, and whether the `DMARC` policy
would reject this server's mail. Each line reports `PASS`, `WARN`, `FAIL` or `INFO`, and any `FAIL` sets a non-zero
exit status, so it can gate a deployment. It needs `dig` from `bind-utils` (Fedora/RHEL) or `dnsutils`
(Debian/Ubuntu), and `mk check-dns` runs it from the repository. It queries `1.1.1.1` by default rather than the
host's resolver: a machine that published a record moments ago can still be answering from a cached negative
lookup, and a public resolver is closer to what receivers see. Use `--resolver system` for the local view, or another
address to check from a different vantage point.

Without a clone, pipe it to `bash` the same way as the installer. Arguments go after `-s --`, and pinning a release tag
instead of `master` keeps the check from changing under you:

```sh
curl -fsSL https://raw.githubusercontent.com/ffimnsr/jmail/master/scripts/check-dns.sh | bash
curl -fsSL https://raw.githubusercontent.com/ffimnsr/jmail/master/scripts/check-dns.sh \
  | bash -s -- --no-port-check --server-ip 203.0.113.10 example.com
```

## Troubleshooting

- **Authentication fails.** `JMAIL_SMTP_USER` and `JMAIL_SMTP_PASSWORD` are applied when the container starts;
  changing them in an existing container has no effect. Recreate it.
- **`530 5.7.0 Must issue a STARTTLS command first`.** Submission requires TLS. Configure the client for STARTTLS on
  port `587` (or the published port); plaintext submission is refused on purpose.
- **`554 5.7.1 Sender address rejected`.** The `From:` address is not at `JMAIL_ORIGIN` or a domain in `JMAIL_DOMAINS`.
  Add the domain to `JMAIL_DOMAINS`, which gives it a DKIM key too, or send as a domain jmail signs. `RCPT TO` reports
  the rejection because `smtpd_delay_reject = yes`.
- **Messages are deferred.** With `smtp_tls_security_level = encrypt`, jmail refuses to deliver in plaintext. Check
  the log for the remote server's TLS error.
- **`postfix status` from inside the container** reports the running state, and
  `s6-rc -a list` shows the supervised services (`svc-postfix`, `svc-opendkim`, `svc-saslauthd`, `svc-syslog`).
- **A record you just published still reports as missing.** Resolvers cache negative answers for the zone's SOA
  minimum, which Cloudflare sets to 30 minutes, so the machine that publishes a record can be the last to see it.
  `check-dns.sh` avoids that by querying `1.1.1.1`; if you asked for the local view with `--resolver system`, flush it
  with `sudo resolvectl flush-caches`.
- **DKIM fails with "value contains literal quote characters".** The DNS provider stored the quotation marks from
  `sendonly.txt` as part of the value, so receivers read a malformed key record. Paste the record unquoted, on one
  line, and let the provider split it into 255-byte strings: `check-dns.sh` joins those back together before it
  compares, so either quoting style shows up correctly in `dig`.
