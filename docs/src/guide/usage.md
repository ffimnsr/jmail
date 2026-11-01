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
(`smtpd_tls_auth_only = yes`), so clients must upgrade the connection before logging in.

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

The scripts in the repository root, `test-auth.py` and `test-auth-send-mail.py`, connect over STARTTLS,
authenticate and (in the second case) send a message. Point `smtp_server` at your server before running them.

## TLS

On first boot jmail generates a self-signed ECDSA (P-384) certificate valid for ten years, with `JMAIL_ORIGIN`
as its common name and subject alternative name. The certificate and key live in `/etc/postfix/ssl/` inside the
`/etc/postfix` volume.

To use a real certificate, put your own `server.crt` and `server.key` in that directory and restart the container.
jmail only generates a certificate when the files are absent.

To regenerate the self-signed certificate (for example after changing `JMAIL_ORIGIN`), create the trigger file and
restart:

```sh
touch /etc/postfix/ssl/.generate   # inside the /etc/postfix volume
podman restart jmail
```

## DKIM

A 2048-bit RSA key is generated for `JMAIL_ORIGIN` on first boot and stored in
`/etc/opendkim/keys/$JMAIL_ORIGIN/sendonly.private`. The matching DNS record is printed to the log as
`sendonly._domainkey IN TXT (...)`, and is also written to `sendonly.txt` next to the key.

Every message sent through jmail is signed as `sendonly._domainkey.$JMAIL_ORIGIN`.

To rotate the key, create the trigger file and restart:

```sh
touch /etc/opendkim/keys/$JMAIL_ORIGIN/.generate   # inside the /etc/opendkim/keys volume
podman restart jmail
```

A new key replaces the old one, so update the DKIM record in DNS before the next message goes out.

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
unquoted string and reject records over 255 characters. If yours does, either upgrade to a provider that supports
long TXT records or switch the key to 1024 bits by editing the two `opendkim-genkey` calls in
`/etc/s6-overlay/s6-rc.d/init-mailer-config/run` and rebuilding the image.

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

Before sending production mail, work through this list:

- [ ] `A` record pointing to your server IP.
  - [ ] `PTR` record on the reverse zone, if you are not using your rDNS hostname as the mail server hostname.
- [ ] `MX` record pointing to the `A` record.
- [ ] `DKIM` record from the jmail log.
- [ ] `SPF` record on your domain. The example below is a hard fail, not a soft fail:
  ```
  v=spf1 a:<mail-server-hostname> ip4:<server-ip> -all
  ```
- [ ] `DMARC` record. This example rejects on the main domain and on subdomains:
  ```
  v=DMARC1; p=reject; sp=reject; rua=mailto:catchall@<mail-server-hostname>
  ```
- [ ] Check whether your IP is on an email blacklist, and delist it if it is.
- [ ] Send a test message to https://mail-tester.com and check the report.

## Troubleshooting

- **Authentication fails.** `JMAIL_SMTP_USER` and `JMAIL_SMTP_PASSWORD` are applied when the container starts;
  changing them in an existing container has no effect. Recreate it.
- **Messages are deferred.** With `smtp_tls_security_level = encrypt`, jmail refuses to deliver in plaintext. Check
  the log for the remote server's TLS error.
- **`postfix status` from inside the container** reports the running state, and
  `s6-rc -a list` shows the supervised services (`svc-postfix`, `svc-opendkim`, `svc-saslauthd`, `svc-syslog`).
