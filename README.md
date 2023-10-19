# jmail

jmail is a simple and lightweight mail server in a container. It's an SMTP-only mail server, so no incoming mails only outgoing. You can connect your servers and APIs to send emails for development and production.

## Features
- Send SMTP mails for development and production use.
- Outgoing emails signed with DKIM.
- Lightweight, customizable and built with security in mind.

## Installation & Usage

```
mkdir -p ./{postfix-data,postfix-config,opendkim-keys}
podman run -d --name jmail -p 5587:587 \
  -h example.com \
  -e JMAIL_HOSTNAME=example.com \
  -e JMAIL_ORIGIN=example.com \
  -e JMAIL_ROOT_EMAIL=user@example.com \
  -v ./postfix-config:/etc/postfix:z \
  -v ./opendkim-keys:/etc/opendkim/keys:z \
  -v ./postfix-data:/var/spool/postfix:z \
  ghcr.io/ffimnsr/jmail:latest
```

## License
jmail is licensed under the Apache-2.0 License. See [LICENSE](LICENSE) file for more details.
