# Multiple domains

By default jmail signs exactly one domain, `JMAIL_ORIGIN`. `JMAIL_DOMAINS` adds more, and each one gets its own DKIM
key, its own signing-table entry and its own certificate SAN.

The shape is deliberately the same one an email service provider uses for its customers: **one sending IP with one
PTR, and one independent set of authentication records per domain**. A receiver's reverse-DNS check is about the IP, so
adding a domain never needs a new A record or a new PTR — it needs TXT records of yours, not new addresses.

## What changes, and what does not

| Thing | With `JMAIL_DOMAINS` |
| --- | --- |
| DKIM key | One per domain, in `/etc/opendkim/keys/<domain>/` |
| Signing table | One `*@<domain>` entry per domain |
| Certificate SAN | Every domain, on the self-signed certificate |
| HTTP API sender policy | Every domain, by default |
| `A` record / `PTR` | **Unchanged** — one pair for the sending IP |
| SASL account | **Unchanged** — one credential, usable for every domain |
| Submission sender policy | Every domain, by default |
| `MX` / inbound mail | **Unchanged** — still none; jmail stays outbound-only |

`JMAIL_ORIGIN` remains the **primary** domain. It is always part of the list and is what gets used wherever a single
name is unavoidable: Postfix `myorigin`, the SASL realm, the certificate CN, and the DKIM report address. Pick the
domain you send the most mail from.

## Configure

```sh
podman run -d --name jmail \
  -h mail.example.com \
  -p 587:587 \
  -e JMAIL_HOSTNAME=mail.example.com \
  -e JMAIL_ORIGIN=one.example \
  -e JMAIL_DOMAINS='two.example, three.example' \
  -e JMAIL_ROOT_EMAIL=postmaster@one.example \
  -e JMAIL_SMTP_USER=jmail \
  -e JMAIL_SMTP_PASSWORD='change-me' \
  -v ./jmail/opendkim-keys:/etc/opendkim/keys:z \
  ghcr.io/ffimnsr/jmail:latest
```

The value may be comma separated, space separated, or both. Entries are lowercased, trimmed of a leading `@` and a
trailing dot, and de-duplicated, so `one.example, ONE.example.` is one domain. An entry that is not a plausible
hostname stops the container at startup with a message naming it, rather than leaving OpenDKIM with a table it cannot
use.

Two practical notes:

- **Subdomains are exact matches.** `SubDomains no` is set, so `mail.three.example` is only signed if you list
  `mail.three.example` itself. Listing `three.example` does not cover it.
- **Environment is read at start.** Changing `JMAIL_DOMAINS` on an existing container has no effect; recreate it.

`scripts/quadlet.sh` passes `JMAIL_DOMAINS` through to `jmail.env` when it is set.

## Sending from any configured domain

A message is signed with the key of the domain in its `From:` header, using selector `sendonly`. So with
`one.example`, `two.example` and `three.example` configured:

```json
{ "from": "noreply@three.example", "to": ["customer@example.net"], "subject": "Hi", "text": "Hello" }
```

is signed as `d=three.example; s=sendonly`, using a key that only exists for that domain. The HTTP API accepts it
without extra configuration, because its default policy is the same list. If you set `JMAIL_HTTP_ALLOWED_FROM`
explicitly it overrides that default, and anything outside it is refused with `422 sender_not_allowed` instead of
being sent unsigned.

Over plain SMTP submission there is no sender check at all: the client may put any `From:` in the message. A domain in
the list is signed, anything else is passed through **unsigned** — which is worth watching for, because an unsigned
message usually still gets delivered, just with worse outcomes.

## DNS per domain

This is the part that stays yours. For each domain in the list, publish three records. Replacing `three.example` with
each of your domains in turn:

```
; DNS for three.example

; 1. DKIM — printed by the container at startup, or read from
;    /etc/opendkim/keys/three.example/sendonly.txt
sendonly._domainkey.three.example. 3600 IN TXT (
  "v=DKIM1; k=rsa; p=MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA..."
  "...rest of the 2048-bit key, split into 255-character strings"
)

; 2. SPF — the IP that actually opens the connection to the recipient
three.example. 3600 IN TXT "v=spf1 ip4:203.0.113.10 -all"

; 3. DMARC — start permissive, tighten once the reports are clean
_dmarc.three.example. 3600 IN TXT "v=DMARC1; p=none; rua=mailto:dmarc@three.example"
```

Three details make the difference between "signed" and "accepted":

- **The DKIM record must be live before mail from that domain goes out.** The key is generated the first time the
  container starts with the domain configured, and mail can be sent immediately. Publish the record first, or hold
  that domain's traffic until it is published.
- **SPF belongs on the domain you send `From:` from.** If your clients set the envelope sender to the same address,
  that is all you need. If a client sets a different `Return-Path`, SPF is evaluated against *that* domain instead,
  and alignment then has to come from DKIM — which jmail supplies, since it signs by `From:`.
- **Use `ip4:` rather than `a:` when the name is proxied.** If `three.example` points at a website behind Cloudflare,
  `a:three.example` authorizes Cloudflare's edge addresses, not your mail server.

What does **not** need to change per domain:

- The `A` record and `PTR` for the sending IP. They are per IP, not per domain, and one IP may send for all of them.
- The `MX` record. Leave it absent, or leave an existing null MX (`v=MX:0 .`) alone. DMARC aggregate reports need a
  mailbox somewhere, so point `rua=` at a reporting service or a mailbox that exists elsewhere.

## Operations

**Adding a domain** needs no downtime for the others:

1. Add it to `JMAIL_DOMAINS` and recreate the container.
2. Watch for `Creating DKIM keys for <domain>` and `Publish this DKIM record at sendonly._domainkey.<domain>` in the
   log, then publish that TXT, the SPF record, and DMARC.
3. Only then start sending `From:` that domain.

**Rotating one domain** leaves the others alone:

```sh
touch /etc/opendkim/keys/three.example/.generate   # inside the /etc/opendkim/keys volume
podman restart jmail
```

Publish the new record before the next message goes out, because the old key is replaced rather than kept.

**Removing a domain** is just removing it from the list and recreating the container. Its key directory is left on the
volume so a rollback does not need a new key, and it stops being referenced by the signing table.

## Checking it

```sh
podman logs jmail | grep -E 'Signing DKIM for|Publish this DKIM record'   # what is configured and generated
podman exec jmail cat /etc/opendkim/signing.table                          # what OpenDKIM will match
podman exec jmail openssl x509 -in /etc/postfix/ssl/server.crt -noout -ext subjectAltName
```

Then send one message per domain and read the receiving side, since only that proves the DNS is right. In Gmail, *Show
original* should contain `dkim=pass header.d=three.example` and, for the `From:` domain, `spf=pass` and `dmarc=pass`.

| Symptom | Cause |
| --- | --- |
| Message has no `DKIM-Signature` | `From:` domain is not in `JMAIL_DOMAINS`, or it is a subdomain of one that is |
| `422 sender_not_allowed` from the HTTP API | `JMAIL_HTTP_ALLOWED_FROM` is set and does not include the domain |
| `dkim=pass` nowhere, but the log shows a signature added | DKIM TXT missing, at the wrong selector, or split incorrectly |
| `spf=neutral` / `none` at the receiver | SPF record missing on the `From:` domain, or it lists the wrong IP |
| Container stops at startup naming a domain | The entry is not a valid hostname; see the message for the entry |

The certificate SAN list only covers domains that were configured when the certificate was first created. After
changing `JMAIL_DOMAINS`, regenerate it:

```sh
touch /etc/postfix/ssl/.generate
podman restart jmail
```

## See also

- [DKIM](../guide/usage.md#dkim) — how signing and rotation work per domain.
- [HTTP API](../guide/http-api.md#sender-policy) — the sender policy in detail.
- [Environment](../refs/environment.md#environment-variables) — `JMAIL_DOMAINS` and the primary domain.
