# Installation

## Requirements

- Docker or Podman.
- A hostname for the mail server that resolves to the container's public IP address. This becomes `JMAIL_HOSTNAME`.
- A domain to send mail as. This becomes `JMAIL_ORIGIN`.
- Port `587` reachable from the applications that will send mail, plus `25` if your provider allows outbound SMTP.

## Pull the image

```sh
podman pull ghcr.io/ffimnsr/jmail:latest
```

Images are published to `ghcr.io/ffimnsr/jmail` for `linux/amd64` and `linux/arm64`, and are signed with cosign.

| Tag | Description |
| --- | --- |
| `latest` | Newest release |
| `X.Y.Z` | Exact release, for example `1.0.14` |
| `X.Y` | Newest patch of a minor release |
| `X` | Newest release of a major version |

## Run the container

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

If you use Docker, replace `podman` with `docker` and drop the `:z` suffixes on the volume mounts.

Set `-h` to the same value as `JMAIL_HOSTNAME`: Postfix uses the hostname in its banner and receivers check its PTR
record, while the generated TLS certificate uses `JMAIL_ORIGIN` as its common name and subject alternative name.

If port `587` is already in use, or you run rootless, publish a higher host port instead, for example
`-p 10587:587`, and point your clients at that port.

### Volumes

| Path | Purpose |
| --- | --- |
| `/etc/postfix/ssl` | The TLS certificate and key. Persist this, otherwise every new container gets a new self-signed certificate. |
| `/etc/opendkim/keys` | DKIM private keys, one directory per domain. |
| `/var/spool/postfix` | Postfix queue. Persisting it keeps mail that is still being retried across restarts. |
| `/etc/jmail/postfix.d`, `/etc/jmail/init.d` | Optional overrides and hooks; see [Customization](../advanced/customization.md). |

`/etc/postfix` as a whole is also a volume in the image, so mounting it instead of `/etc/postfix/ssl` works too.
Its rendered files are rewritten from the templates on every start, which is why only `ssl/` is worth persisting.

### Ports

| Port | Purpose |
| --- | --- |
| `25` | SMTP with postscreen in front of it. |
| `587` | Submission with STARTTLS and SASL authentication. This is the port the image declares with `EXPOSE`. |

Most deployments only publish `587`, for example `-p 10587:587` if the host already uses `587`. Publishing port
`25` is only useful if another mail server should relay through jmail.

### Check the logs

```sh
podman logs -f jmail
```

On first boot the container creates its configuration, prints the DKIM public key it generated, and prints the
self-signed certificate so you can inspect it. Once Postfix is up you should see
`daemon started -- version 3.x.x, configuration /etc/postfix`.

A quick way to confirm the SMTP ports are accepting connections:

```sh
podman exec jmail nc -z 127.0.0.1 25
podman exec jmail nc -z 127.0.0.1 587
podman healthcheck run jmail
```

Use `127.0.0.1` rather than `localhost`: Postfix is configured with `inet_protocols = ipv4`, so `localhost` may
resolve to `::1` and the check would fail even when the service is up.

## Podman quadlet

If you run Podman with systemd, a quadlet is more convenient than `podman run`. Store it as
`/etc/containers/systemd/jmail.container` and adjust the environment values and paths:

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

Then `systemctl daemon-reload` and `systemctl start jmail`.

The image declares a `HEALTHCHECK`, which Docker, Compose and Kubernetes pick up automatically. Podman does not
propagate image health checks into containers, so the quadlet above declares the same check itself. Note the
explicit `127.0.0.1`: Postfix runs with `inet_protocols = ipv4`, so `localhost` may resolve to `::1` and the check
would fail even when the service is up.

### Generating the quadlet

The repository ships a generator for the unit and its environment file:

```sh
./scripts/quadlet.sh                  # writes quadlet/jmail.container and quadlet/jmail.env
./scripts/quadlet.sh --install        # user scope: ~/.config/containers/systemd, systemctl --user
sudo ./scripts/quadlet.sh --scope system --install
```

It reads `JMAIL_IMAGE`, `JMAIL_HOSTNAME`, `JMAIL_ORIGIN`, `JMAIL_ROOT_EMAIL`, `JMAIL_SMTP_USER`,
`JMAIL_SMTP_PASSWORD`, `JMAIL_MYNETWORKS` and `JMAIL_PUBLISH_PORT` from the environment, generates a random SMTP
password when none is given, and writes the environment file with `0600` permissions. `--help` lists the defaults
for each variable.

## Upgrading

The container regenerates every rendered file from its templates on each start, so pulling a newer image and
recreating the container is usually all an upgrade needs:

```sh
podman pull ghcr.io/ffimnsr/jmail:latest
podman rm -f jmail
# re-run the podman run (or quadlet) command with the same volumes and environment
```

Keep the `/etc/opendkim/keys` volume: your existing DKIM keys stay valid and your DNS records do not change.

## Working from the repository

The checkout ships [`mk`](https://github.com/ffimnsr/mk-rs) tasks for the local loop, so you do not have to paste
the `podman run` line above while developing:

```sh
mk build          # build the image locally, labelled with the newest tag
mk up             # start a dev container on 10587, state under .jmail/
mk logs           # follow it
mk test-auth      # STARTTLS + AUTH check against 127.0.0.1:10587
mk test-mail      # send the built-in test message
mk clean          # remove the container and the state directory
```

`mk list` shows the rest, including `quadlet-generate`, `docs` and the release tasks.
