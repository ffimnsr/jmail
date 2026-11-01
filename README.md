# jmail

jmail is a simple and lightweight mail server in a container. It's an SMTP-only mail server (Outbound Only), so no incoming mails only outgoing. You can connect your servers and APIs to send emails for development and production.

## Features
- Send SMTP mails for development and production use.
- Outgoing emails signed with DKIM.
- Lightweight, customizable and built with security in mind.

## Installation & Usage

On a clean workstation do pull then run the container.
```
podman pull ghcr.io/ffimnsr/jmail:latest
mkdir -p ./{postfix-data,postfix-config,opendkim-keys}
podman run -d --name jmail -p 5587:587 \
  -h example.com \
  -e JMAIL_HOSTNAME=example.com \
  -e JMAIL_ORIGIN=example.com \
  -e JMAIL_ROOT_EMAIL=user@example.com \
  -e JMAIL_SMTP_USER=jmail \
  -e JMAIL_SMTP_PASSWORD=mysecretpassword \
  -v ./postfix-config:/etc/postfix:z \
  -v ./opendkim-keys:/etc/opendkim/keys:z \
  -v ./postfix-data:/var/spool/postfix:z \
  ghcr.io/ffimnsr/jmail:latest
```
And lastly, check the logs if everything is working fine.
The last thing to do before going to production is to setup correctly your `A`, `SPF`, `DKIM`, and `DMARC` records on your preferred public DNS.

Here is a sample podman quadlet that you can use on your machine if you're utilizing podman architecture:
```
[Unit]
Description=Jmail Service
Wants=network-online.target
After=network-online.target

[Container]
ContainerName=jmail
Image=ghcr.io/ffimnsr/jmail:latest
Pull=always
HostName=<mail-server-hostname>
Volume=/etc/container-configs/jmail/postfix-ssl:/etc/postfix/ssl:z
Volume=/etc/container-configs/jmail/opendkim-keys:/etc/opendkim/keys:z
Volume=/var/container-data/postfix:/var/spool/postfix:z
Environment=JMAIL_HOSTNAME=<mail-server-hostname>
Environment=JMAIL_ORIGIN=<mail-server-hostname or mail-origin>
Environment=JMAIL_ROOT_EMAIL=<root-email>
Environment=JMAIL_SMTP_USER=<username>
Environment=JMAIL_SMTP_PASSWORD=<password>
# Docker uses the image's HEALTHCHECK; podman needs it declared here.
HealthCmd=nc -z 127.0.0.1 587
HealthInterval=30s
HealthRetries=3
HealthStartPeriod=30s
HealthTimeout=5s
PublishPort=10587:587

[Service]
Restart=always

[Install]
WantedBy=multi-user.target default.target
```

## Customization

Two directories in the image exist to be mounted, so most changes do not need a rebuild:

- `/etc/jmail/postfix.d/*.conf` holds `parameter = value` lines applied with `postconf -e` on every start, for
  settings the templates do not expose (smarthost, rate limits, TLS policy).
- `/etc/jmail/init.d/*.sh` holds executable hooks that run once the configuration, certificate, DKIM key and SASL
  account exist, and before Postfix starts.

```
podman run -d --name jmail \
  -v ./postfix.d:/etc/jmail/postfix.d:z \
  -v ./init.d:/etc/jmail/init.d:z \
  ...
```

Everything else, including replacing a template or adding a service to a derived image, is covered in
[docs/src/advanced/customization.md](docs/src/advanced/customization.md).

## Mail Checklist

This are the things you need to do before going to production:

- [ ] Setup `A` record pointing to your server IP.
  - [ ] Setup `PTR` records on your reverse zones if you are not using your rDNS hostname as mail server hostname.
- [ ] Setup `MX` records pointing to the `A` record.
- [ ] Setup `DKIM` to the one printed in the jmail logs.
- [ ] Setup `SPF` to your domain record and or to your ip address. The setup below is full reject/fail (bounce) and not soft fail (accept and record).
  ```
  v=spf1 a:<mail-server-hostname> ip4:<server-ip> -all
  ```
- [ ] Setup `DMARC` to strict. This specifies the policy to reject on main domain then reject also on subdomain.
  ```
  v=DMARC1;  p=reject; sp=reject; rua=mailto:catchall@<mail-server-hostname>
  ```
- [ ] Check if your IP is in email blacklists. De-list it.
- [ ] Test your emails on https://mail-tester.com.


## Tasks

The repository uses [`mk`](https://github.com/ffimnsr/mk-rs) for the day-to-day commands; `mk list` shows all of
them.

| Task | What it does |
| --- | --- |
| `mk build` | Build the image locally, labelled with the newest tag |
| `mk up` | Start a disposable dev container on port `10587` |
| `mk dev` / `mk logs` / `mk stop` | Run in the foreground, follow logs, remove the dev container |
| `mk clean` | Remove the dev container and the local `.jmail/` state directory |
| `mk test-mail` | Send the built-in test message to `JMAIL_ROOT_EMAIL` |
| `mk test-auth` / `mk test-send` | Check STARTTLS + AUTH, then deliver a message through the container |
| `mk dkim` | Print the DKIM TXT record to publish |
| `mk quadlet-generate` | Write `quadlet/jmail.container` and `quadlet/jmail.env` |
| `mk quadlet-install` | Install and start the user-scope quadlet |
| `mk release-patch` | Cut a release; `release-minor` and `release-major` also exist |
| `mk docs` / `mk docs-build` | Serve the documentation locally / build it into `./book` |

## Releasing

Releases are semver tags (`vX.Y.Z`) on `main`:

```
scripts/release.sh --patch     # or --minor, --major, or an explicit version
```

The script runs the release gates, writes the `CHANGELOG.md` section from the conventional commits since the
previous tag, creates the `release: vX.Y.Z` commit, tags it and pushes. Pushing the tag is what builds and
publishes `ghcr.io/ffimnsr/jmail` and deploys the documentation.

## License
jmail is licensed under the Apache-2.0 License. See [LICENSE](LICENSE) file for more details.
