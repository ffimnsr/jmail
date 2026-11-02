# Environment

## Environment variables

| Variable | Default | Used for |
| --- | --- | --- |
| `JMAIL_HOSTNAME` | `example.com` | Postfix `myhostname`: SMTP banner and `smtpd_sasl_local_domain`. |
| `JMAIL_ORIGIN` | `example.com` | The primary domain: Postfix `myorigin`, the default OpenDKIM signing domain, the SASL realm, and the generated TLS certificate's CN. |
| `JMAIL_DOMAINS` | *(unset)* | Additional domains to sign, space or comma separated. `JMAIL_ORIGIN` is always included and stays primary, so a single-domain setup can leave this unset. Together with `JMAIL_ORIGIN` it also forms the `From:` allowlist for authenticated submission. |
| `JMAIL_ROOT_EMAIL` | `user@example.com` | Destination of the `root` alias and of `/usr/local/bin/test-mail.sh`. |
| `JMAIL_SMTP_USER` | `jmail` | SASL username created at startup. |
| `JMAIL_SMTP_PASSWORD` | `mysecretpassword` | SASL password for `JMAIL_SMTP_USER`. |
| `JMAIL_MYNETWORKS` | `127.0.0.0/8` | Clients allowed to relay **without** authenticating (`mynetworks`). Keep it as narrow as possible; see [Usage](../guide/usage.md#smtp-settings). |
| `S6_BEHAVIOUR_IF_STAGE2_FAILS` | `2` | s6-overlay behaviour when a service fails to start. `2` stops the container instead of leaving it with a dead service. |

The defaults exist so the image can be started for a quick test, but they are placeholder values: always set
`JMAIL_HOSTNAME`, `JMAIL_ORIGIN`, `JMAIL_ROOT_EMAIL`, `JMAIL_SMTP_USER` and `JMAIL_SMTP_PASSWORD` in production.

`JMAIL_DOMAINS` is the one exception: it is optional, and listing the same domain twice is harmless because entries are
lowercased, trimmed and de-duplicated. An entry that is not a plausible hostname stops the container at startup rather
than producing a signing table OpenDKIM cannot use. Each domain gets its own DKIM key, its own certificate SAN, and its
own `*@<domain>` signing-table entry; everything that has to be singular (`myorigin`, the SASL realm, the certificate
CN, the DKIM report address) keeps using `JMAIL_ORIGIN`, so the primary domain should be the one you send the most mail
from.

All variables are read when the container starts. Changing them on an existing container has no effect — recreate it.

## HTTP API variables

The HTTP submission API is off unless `JMAIL_HTTP_TOKEN` or `JMAIL_HTTP_SUPABASE_HOOK_SECRET` is set. See
[HTTP API](../guide/http-api.md).

| Variable | Default | Used for |
| --- | --- | --- |
| `JMAIL_HTTP_TOKEN` | *(unset)* | Bearer token for `POST /v1/send`. Empty means that route is not mounted, which is the right choice when the service only receives the Supabase hook. Use `openssl rand -hex 32`. |
| `JMAIL_HTTP_LISTEN` | `0.0.0.0:8080` | Listen address inside the container. |
| `JMAIL_HTTP_ALLOWED_FROM` | `JMAIL_DOMAINS` | Space or comma separated sender domains. `*` accepts any sender, which does **not** make them DKIM-signed. |
| `JMAIL_HTTP_SMTP_ADDR` | `127.0.0.1:25` | Where the service hands the message to Postfix. Loopback is trusted through `mynetworks`. |
| `JMAIL_HTTP_MAX_BYTES` | `5M` | Cap on the request body and on the decoded attachments. Accepts a `K`/`M`/`G` suffix. |
| `JMAIL_HTTP_MAX_RECIPIENTS` | `50` | Cap on `to` + `cc` + `bcc` after de-duplication. |
| `JMAIL_HTTP_MAX_ATTACHMENTS` | `10` | Cap on the number of attachments. |
| `JMAIL_HTTP_CONCURRENCY` | `16` | Simultaneous SMTP submissions. |
| `JMAIL_HTTP_RATE` | `10` | Accepted requests per second, as a token bucket. |
| `JMAIL_HTTP_RATE_BURST` | `20` | Bucket size, so short spikes up to this size pass. |
| `JMAIL_HTTP_TIMEOUT` | `30s` | Overall deadline for one SMTP submission. |
| `JMAIL_HTTP_DRAIN` | `2s` | How long a `SIGTERM` waits for in-flight submissions before exiting. Must stay below `S6_KILL_GRACETIME`. |
| `JMAIL_HTTP_IDEMPOTENCY_TTL` | `1h` | How long an `Idempotency-Key` replay is remembered. |
| `JMAIL_HTTP_IDEMPOTENCY_DIR` | `/var/lib/jmail-http` | Where replay records are written so they survive a restart. `off` keeps them in memory only. |
| `JMAIL_HTTP_LOG_RATE` | `10` | Access-log lines per second, sampled with a token bucket. `0` logs every request. |
| `JMAIL_HTTP_SUPABASE_HOOK_SECRET` | *(unset)* | Enables `POST /v1/hooks/supabase/send-email` for one project. Accepts the dashboard's `v1,whsec_<base64>` value or a bare base64 secret. |
| `JMAIL_HTTP_SUPABASE_URL` | *(unset)* | Project API URL used to build confirmation links in those emails, for example `https://abcdefgh.supabase.co`. Without it the email carries only the numeric code. |
| `JMAIL_HTTP_SUPABASE_FROM` | `noreply@JMAIL_ORIGIN` | Sender for Supabase hook emails. Must be within `JMAIL_HTTP_ALLOWED_FROM`. |
| `JMAIL_HTTP_SUPABASE_<N>_HOOK_SECRET` | *(unset)* | A second, third, … project, with `N` from 1 to 16, and `JMAIL_HTTP_SUPABASE_<N>_URL` / `_FROM` alongside it. Each project signs with its own secret; the one that verifies selects that project's URL and sender. |

## Volumes

| Path | Contents |
| --- | --- |
| `/etc/postfix` | Rendered Postfix configuration, `aliases`, `canonical`, `generic` databases and `ssl/server.{crt,key}`. |
| `/etc/opendkim/keys` | One DKIM private key per signed domain, at `/etc/opendkim/keys/<domain>/sendonly.private`. |
| `/var/spool/postfix` | Postfix queue. |
| `/var/lib/jmail-http` | HTTP API idempotency records, so a client retry still replays after a restart. Only written when the API is enabled. |

Two more directories are meant to be mounted but are not volumes, so they stay empty unless you fill them:

| Path | Contents |
| --- | --- |
| `/etc/jmail/postfix.d` | `*.conf` files of `parameter = value` lines applied with `postconf -e` on every start. |
| `/etc/jmail/init.d` | Executable `*.sh` hooks run after configuration, before services start. |

`/var/lib/jmail-http` is declared with `VOLUME`, so removing the container without `-v` leaves the records in an
anonymous volume instead of deleting them. That keeps them from being lost by accident, but an anonymous volume is
hard to find again, so prefer a named volume or a bind mount when the records matter:

```sh
-v jmail-http-state:/var/lib/jmail-http     # named volume
-v ./jmail/http-state:/var/lib/jmail-http:z # bind mount, SELinux relabelled
```

See [Customization](../advanced/customization.md).

## Ports

| Port | Service |
| --- | --- |
| `25` | SMTP in front of postscreen. |
| `587` | Submission with STARTTLS and SASL authentication (declared with `EXPOSE`). |
| `8080` | HTTP API, plain HTTP, serving when `JMAIL_HTTP_TOKEN` or `JMAIL_HTTP_SUPABASE_HOOK_SECRET` is set. |
| `8892` | OpenDKIM milter socket, bound to `127.0.0.1` only. |

## Files in the image

| Path | Description |
| --- | --- |
| `/etc/templates` | Source templates for every rendered file. |
| `/etc/postfix/main.cf`, `master.cf` | Rendered on every start from `/etc/templates/postfix`. |
| `/etc/opendkim/opendkim.conf`, `key.table`, `signing.table`, `trusted.hosts` | Rendered on every start. |
| `/etc/sasl2/smtpd.conf`, `/etc/syslog.conf` | Rendered on every start. |
| `/usr/local/bin/test-mail.sh` | Generated helper that mails `JMAIL_ROOT_EMAIL`. |
| `/usr/local/bin/jmail-http` | Static binary serving the optional HTTP API. |
| `/usr/local/bin/jmail-http-enabled` | Succeeds when the HTTP API is configured (token or any Supabase hook secret); used by the s6 service and the health check. |
| `/etc/jmail/postfix.d` | User overrides applied on top of the rendered `main.cf`. |
| `/etc/jmail/init.d` | User hooks, run last by `init-mailer-config`. |
| `/etc/postfix/ssl/.generate` | Presence triggers regeneration of the self-signed certificate on the next start. |
| `/etc/opendkim/keys/<domain>/.generate` | Presence triggers a new DKIM key for that domain on the next start. |

## Services

s6-overlay supervises one oneshot and five long-running services. `s6-rc -a list` lists them, and the per-service
scripts live under `/etc/s6-overlay/s6-rc.d`:

| Service | Purpose |
| --- | --- |
| `init-mailer-config` | Oneshot: renders templates, applies `/etc/jmail/postfix.d`, creates the certificate, DKIM key and SASL account, then runs `/etc/jmail/init.d` hooks. |
| `svc-syslog` | `syslogd`, so Postfix and OpenDKIM logs reach the container output. |
| `svc-opendkim` | OpenDKIM milter. |
| `svc-saslauthd` | `saslauthd` with the `sasldb` backend. |
| `svc-postfix` | Postfix in the foreground. |
| `svc-jmail-http` | HTTP API. Parks itself when no HTTP setting is present — no token and no Supabase hook secret, numbered or not — because a static s6 bundle cannot be conditional. |

s6 restarts a service whenever it exits, which is what recovers `svc-jmail-http` from a
crash. Two details make that survivable:

- A `finish` script sleeps one second after a failed exit, so a binary that fails
  immediately (bad configuration, port already bound) restarts about once a second instead
  of spinning.
- `s6-overlay` sends `SIGTERM`, waits `S6_KILL_GRACETIME` (3000 ms by default, a fixed
  delay rather than a ceiling), then sends `SIGKILL`. `JMAIL_HTTP_DRAIN` therefore defaults
  to `2s` so the graceful path finishes inside it. Raising one without the other only
  lengthens every `podman stop` or `docker stop`.

Services are listed in `/etc/s6-overlay/user-bundles.d/user/contents.d`, which is the s6-overlay 3.2.3+ bundle
location.

## Health check

The image declares its own `HEALTHCHECK`, so Docker, Compose and Kubernetes can restart a wedged container without
extra configuration:

```
postfix status >/dev/null 2>&1 && nc -z 127.0.0.1 587 && \
  { ! /usr/local/bin/jmail-http-enabled || nc -z 127.0.0.1 8080; }
```

It runs every 30 seconds after a 30-second start period. The HTTP port is only checked
when `JMAIL_HTTP_TOKEN` is set, because the API parks itself otherwise and the port stays
closed. `127.0.0.1` is spelled out on purpose: Postfix runs with
`inet_protocols = ipv4`, so `localhost` can resolve to `::1` and the check would always fail.

Podman does not copy an image's health check into the container it creates, so with podman you have to declare the
same command in a quadlet; see [Installation](../guide/installation.md#podman-quadlet).
