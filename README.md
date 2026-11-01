# jmail

jmail is a simple and lightweight mail server in a container. It's an SMTP-only mail server (Outbound Only), so no incoming mails only outgoing. You can connect your servers and APIs to send emails for development and production.

## Features
- Send SMTP mails for development and production use.
- Outgoing emails signed with DKIM.
- Lightweight, customizable and built with security in mind.

## Installation

Pull the image:

```sh
podman pull ghcr.io/ffimnsr/jmail:latest
```

Images are published for `linux/amd64` and `linux/arm64`, tagged `latest`, `X.Y.Z`, `X.Y` and `X`, and signed with
cosign.

### Quick start

```sh
mkdir -p ./jmail/{postfix-ssl,opendkim-keys,spool}
podman run -d --name jmail \
  --restart unless-stopped \
  -h mail.example.com \
  -p 587:587 \
  -e JMAIL_HOSTNAME=mail.example.com \
  -e JMAIL_ORIGIN=example.com \
  -e JMAIL_ROOT_EMAIL=postmaster@example.com \
  -e JMAIL_SMTP_USER=jmail \
  -e JMAIL_SMTP_PASSWORD='change-me' \
  -v ./jmail/postfix-ssl:/etc/postfix/ssl:z \
  -v ./jmail/opendkim-keys:/etc/opendkim/keys:z \
  -v ./jmail/spool:/var/spool/postfix:z \
  ghcr.io/ffimnsr/jmail:latest
```

- `-h` must match `JMAIL_HOSTNAME`. Postfix announces it in the SMTP banner and receivers check its PTR record.
- `JMAIL_ORIGIN` is the domain that gets DKIM-signed, and the realm the SASL user is created in. Only `From:`
  addresses at this domain are signed.
- If port `587` is taken, or you run rootless, publish a higher port instead, for example `-p 10587:587`.
- On Docker, use `docker run` and drop the `:z` suffixes. Those relabel the mounts for SELinux.

Then watch the logs:

```sh
podman logs -f jmail
```

On first boot the container renders its configuration, generates a 2048-bit DKIM key and a self-signed ECDSA
certificate, creates the SASL account, and prints the DKIM TXT record you have to publish.

### Send through it

| Setting | Value |
| --- | --- |
| Host | Your server, or `127.0.0.1` from the same machine |
| Port | `587` |
| Encryption | STARTTLS, required before `AUTH` (`smtpd_tls_auth_only = yes`) |
| Username | `JMAIL_SMTP_USER` |
| Password | `JMAIL_SMTP_PASSWORD` |

From inside the container, `podman exec jmail test-mail.sh` sends a message to `JMAIL_ROOT_EMAIL`. Relaying is only
allowed for authenticated clients and for the networks in `JMAIL_MYNETWORKS`, which defaults to `127.0.0.0/8`.

### Podman quadlet

For a systemd-managed deployment, generate the unit instead of writing one by hand:

```sh
./scripts/quadlet.sh --scope system --install     # root, /etc/containers/systemd
./scripts/quadlet.sh --install                    # user scope, systemctl --user
```

It writes the `.container` unit plus a `0600` environment file, generates an SMTP password when you do not supply
`JMAIL_SMTP_PASSWORD`, and starts the service. Drop `--install` to only generate the files. The resulting unit, and
the manual alternative, are documented in
[docs/src/guide/installation.md](docs/src/guide/installation.md#podman-quadlet).

Full documentation: [`docs/`](docs) (published from the repository).

## Mail Checklist

These are the things to get right before you send production mail. Every one of them is checked from the receiving
side, so passing a local test send is not enough.

- [ ] `A` record for `JMAIL_HOSTNAME` pointing at the server's public IP.
- [ ] `PTR` (rDNS) record on that IP resolving back to the same name. Receivers compare it with the banner hostname,
      and a mismatch costs you deliverability even when everything else is correct. This is set at your hosting
      provider, not in your DNS zone.
- [ ] **No `MX` record is needed.** jmail is outbound-only: it does not accept inbound mail, so an MX record would
      only advertise a service this container does not run. If the domain already has a null MX (`v=MX:0 .`), leave
      it alone.
- [ ] `DKIM` — publish the record printed at startup, at `sendonly._domainkey.<JMAIL_ORIGIN>`. It is also written to
      `/etc/opendkim/keys/<JMAIL_ORIGIN>/sendonly.txt`, which you can read from the running container with
      `podman exec jmail sh -c 'cat /etc/opendkim/keys/$JMAIL_ORIGIN/sendonly.txt'`. A 2048-bit key produces
      roughly 400 characters, which has to stay split into quoted 255-character strings on one line.
- [ ] `SPF` — on the domain you send `From:`, which for this image means `JMAIL_ORIGIN`:
      ```
      v=spf1 a:<mail-server-hostname> ip4:<server-ip> -all
      ```
      `-all` is a hard fail; that is what you want once the record is correct. A `From:` address at any other
      domain goes out unsigned and unaligned, so either keep senders inside `JMAIL_ORIGIN` or extend the OpenDKIM
      signing table (see [Customization](docs/src/advanced/customization.md)).
- [ ] Outbound port `25` is not blocked. Many cloud and VPS providers block it by default; mail then queues with a
      `connect to ...:25` error instead of being delivered.
- [ ] `DMARC` — start permissive so you can read the reports, then tighten:
      ```
      v=DMARC1; p=none; rua=mailto:dmarc@<your-domain>
      ```
      After a week or two of clean reports, move to `p=reject`, and add `sp=reject` if subdomains should be rejected
      too:
      ```
      v=DMARC1; p=reject; sp=reject; rua=mailto:dmarc@<your-domain>
      ```
- [ ] TLS — the container serves a self-signed certificate until you put a real `server.crt` and `server.key` into
      the `/etc/postfix/ssl` volume. Swap them in if your clients or upstream relays verify certificates.
- [ ] Check the IP against the major blocklists, for example https://multirbl.valli.org, and request delisting
      before the first send.
- [ ] Send a real message to https://www.mail-tester.com and aim for a clean report before rolling out.

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
