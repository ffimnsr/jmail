# Environment

## Environment variables

| Variable | Default | Used for |
| --- | --- | --- |
| `JMAIL_HOSTNAME` | `example.com` | Postfix `myhostname`: SMTP banner and `smtpd_sasl_local_domain`. |
| `JMAIL_ORIGIN` | `example.com` | Postfix `myorigin`, the OpenDKIM signing `Domain`, the SASL realm, and the generated TLS certificate's CN/SAN. |
| `JMAIL_ROOT_EMAIL` | `user@example.com` | Destination of the `root` alias and of `/usr/local/bin/test-mail.sh`. |
| `JMAIL_SMTP_USER` | `jmail` | SASL username created at startup. |
| `JMAIL_SMTP_PASSWORD` | `mysecretpassword` | SASL password for `JMAIL_SMTP_USER`. |
| `JMAIL_MYNETWORKS` | `127.0.0.0/8` | Clients allowed to relay **without** authenticating (`mynetworks`). Keep it as narrow as possible; see [Usage](../guide/usage.md#smtp-settings). |
| `S6_BEHAVIOUR_IF_STAGE2_FAILS` | `2` | s6-overlay behaviour when a service fails to start. `2` stops the container instead of leaving it with a dead service. |

The defaults exist so the image can be started for a quick test, but they are placeholder values: always set
`JMAIL_HOSTNAME`, `JMAIL_ORIGIN`, `JMAIL_ROOT_EMAIL`, `JMAIL_SMTP_USER` and `JMAIL_SMTP_PASSWORD` in production.

All variables are read when the container starts. Changing them on an existing container has no effect — recreate it.

## Volumes

| Path | Contents |
| --- | --- |
| `/etc/postfix` | Rendered Postfix configuration, `aliases`, `canonical`, `generic` databases and `ssl/server.{crt,key}`. |
| `/etc/opendkim/keys` | DKIM private keys at `/etc/opendkim/keys/$JMAIL_ORIGIN/sendonly.private`. |
| `/var/spool/postfix` | Postfix queue. |

Two more directories are meant to be mounted but are not volumes, so they stay empty unless you fill them:

| Path | Contents |
| --- | --- |
| `/etc/jmail/postfix.d` | `*.conf` files of `parameter = value` lines applied with `postconf -e` on every start. |
| `/etc/jmail/init.d` | Executable `*.sh` hooks run after configuration, before services start. |

See [Customization](../advanced/customization.md).

## Ports

| Port | Service |
| --- | --- |
| `25` | SMTP in front of postscreen. |
| `587` | Submission with STARTTLS and SASL authentication (declared with `EXPOSE`). |
| `8892` | OpenDKIM milter socket, bound to `127.0.0.1` only. |

## Files in the image

| Path | Description |
| --- | --- |
| `/etc/templates` | Source templates for every rendered file. |
| `/etc/postfix/main.cf`, `master.cf` | Rendered on every start from `/etc/templates/postfix`. |
| `/etc/opendkim/opendkim.conf`, `key.table`, `signing.table`, `trusted.hosts` | Rendered on every start. |
| `/etc/sasl2/smtpd.conf`, `/etc/syslog.conf` | Rendered on every start. |
| `/usr/local/bin/test-mail.sh` | Generated helper that mails `JMAIL_ROOT_EMAIL`. |
| `/etc/jmail/postfix.d` | User overrides applied on top of the rendered `main.cf`. |
| `/etc/jmail/init.d` | User hooks, run last by `init-mailer-config`. |
| `/etc/postfix/ssl/.generate` | Presence triggers regeneration of the self-signed certificate on the next start. |
| `/etc/opendkim/keys/$JMAIL_ORIGIN/.generate` | Presence triggers a new DKIM key on the next start. |

## Services

s6-overlay supervises one oneshot and four long-running services. `s6-rc -a list` lists them, and the per-service
scripts live under `/etc/s6-overlay/s6-rc.d`:

| Service | Purpose |
| --- | --- |
| `init-mailer-config` | Oneshot: renders templates, applies `/etc/jmail/postfix.d`, creates the certificate, DKIM key and SASL account, then runs `/etc/jmail/init.d` hooks. |
| `svc-syslog` | `syslogd`, so Postfix and OpenDKIM logs reach the container output. |
| `svc-opendkim` | OpenDKIM milter. |
| `svc-saslauthd` | `saslauthd` with the `sasldb` backend. |
| `svc-postfix` | Postfix in the foreground. |

Services are listed in `/etc/s6-overlay/user-bundles.d/user/contents.d`, which is the s6-overlay 3.2.3+ bundle
location.

## Health check

The image declares its own `HEALTHCHECK`, so Docker, Compose and Kubernetes can restart a wedged container without
extra configuration:

```
postfix status >/dev/null 2>&1 && nc -z 127.0.0.1 587
```

It runs every 30 seconds after a 30-second start period. `127.0.0.1` is spelled out on purpose: Postfix runs with
`inet_protocols = ipv4`, so `localhost` can resolve to `::1` and the check would always fail.

Podman does not copy an image's health check into the container it creates, so with podman you have to declare the
same command in a quadlet; see [Installation](../guide/installation.md#podman-quadlet).
