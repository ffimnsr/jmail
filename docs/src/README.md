# Introduction

jmail is a simple, lightweight outbound-only SMTP server in a container. It runs
[Postfix](https://www.postfix.org/) under [s6-overlay](https://github.com/just-containers/s6-overlay) and signs every
outgoing message with DKIM using [OpenDKIM](http://www.opendkim.org/).

There is no inbound mail handling: no mailboxes, no IMAP, no POP3. You point an application or another server at it
and it relays your messages to the destination mail server.

## Features

- Authenticated SMTP submission with STARTTLS (`PLAIN`, `LOGIN`, `CRAM-MD5`, `DIGEST-MD5`, `NTLM`).
- Outgoing mail signed with DKIM, for one domain or several.
- Self-signed TLS certificate generated on first boot, easy to replace with your own.
- Configuration driven by environment variables and templates, so you can reconfigure without rebuilding.
- Multi-arch images for `linux/amd64` and `linux/arm64`.

## Documentation

- [Installation](guide/installation.md) — pull, run and configure the container.
- [Usage](guide/usage.md) — send mail, manage TLS and DKIM keys, DNS checklist.
- [Environment reference](refs/environment.md) — every variable, volume and port the image uses.
