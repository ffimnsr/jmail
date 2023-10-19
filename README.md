# jmail

jmail is a simple and lightweight mail server in a container. It's an SMTP-only mail server, so no incoming mails only outgoing. You can connect your servers and APIs to send emails for development and production.

## Features
- Send SMTP mails for development and production use.
- Outgoing emails signed with DKIM.
- Lightweight, customizable and built with security in mind.

## Installation & Usage

```
podman run -d --name memos -p 5587:587 \
  -v ~/.memos/:/var/opt/memos \
  ghcr.io/ffimnsr/jmail:latest
```

## License
jmail is licensed under the Apache-2.0 License. See LICENSE file for more details.
