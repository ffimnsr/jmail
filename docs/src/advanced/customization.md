# Customization

jmail renders its configuration from templates on **every** start:

```
/etc/templates -> dockerize -> /etc/postfix/main.cf, /etc/opendkim/opendkim.conf, ...
```

That has one consequence worth internalising: editing a rendered file does not stick. A `main.cf` you write by
hand, or mount over `/etc/postfix/main.cf`, is replaced on the next start. Customization therefore happens in one of
four places, from cheapest to most involved.

| You want to | Use |
| --- | --- |
| Change a value the image already exposes | environment variable, see [Environment](../refs/environment.md#environment-variables) |
| Change a Postfix setting the templates do not expose | a file in `/etc/jmail/postfix.d/` |
| Run commands once everything is set up | a script in `/etc/jmail/init.d/` |
| Change the template itself, or add a service | a derived image |

The first three need no image build. `/etc/jmail/init.d` and `/etc/jmail/postfix.d` are ordinary directories in the
image, so you can bind-mount them, or `COPY` files into them from a derived image.

There is deliberately no interactive setup: containers start unattended under `podman run -d`, quadlets, Compose and
Kubernetes, where a prompt cannot be answered. If you want a wizard, generate an env file or quadlet on the host and
feed the result to the container.

## Postfix overrides

Any `*.conf` file in `/etc/jmail/postfix.d/` is applied with `postconf -e` after the templates are rendered and
before Postfix starts. One `parameter = value` per line; `#` starts a comment and blank lines are ignored. Files are
applied in filename order, so a later file wins over an earlier one.

A smarthost is the usual reason to reach for this:

```sh
/etc/jmail/postfix.d/10-smarthost.conf

# Send everything through the provider instead of direct-to-MX.
relayhost = [smtp.example.net]:587
smtp_tls_security_level = encrypt
```

```sh
podman run -d --name jmail \
  -v /srv/jmail/postfix.d:/etc/jmail/postfix.d:z \
  ... \
  ghcr.io/ffimnsr/jmail:latest
```

Rate limits are another common one, and worth setting if an SMTP credential ever leaks:

```sh
smtpd_client_message_rate_limit = 60
smtpd_client_connection_rate_limit = 30
```

An override that does not apply is treated as an error. `postconf` itself only warns about an unknown parameter and
still exits 0, so jmail validates each name first and stops the container with a message naming the file:

```
Applying Postfix overrides from /etc/jmail/postfix.d/10-bad.conf
error: /etc/jmail/postfix.d/10-bad.conf: postconf: warning: no_such_parameter: unknown parameter
```

Values are not type-checked beyond what `postconf` does; a syntactically valid but wrong value (say,
`inet_protocols = ipv9`) shows up as Postfix failing to start rather than as a bad override.

Overrides are re-applied on every start and are not idempotent-hostile, so keep them declarative. Anything that needs
to compute a value belongs in a hook.

## Hooks

Executable `*.sh` files in `/etc/jmail/init.d/` run once per container start, in filename order, **after** the
configuration is rendered and the TLS certificate, DKIM key and SASL account exist, and **before** any service
starts. They run as `root`, inherit the container environment (including the `JMAIL_*` variables), and print to the
container log.

```sh
/etc/jmail/init.d/20-virtual-aliases.sh

#!/usr/bin/env bash
set -e

# Rewrite every sender to the verified domain, whatever the application sends.
printf 'smtpsender@example.com\tbounce@example.com\n' > /etc/postfix/generic
postmap /etc/postfix/generic
postconf -e 'smtp_generic_maps = lmdb:/etc/postfix/generic'
```

A hook that exits non-zero fails `init-mailer-config`, and because the image sets
`S6_BEHAVIOUR_IF_STAGE2_FAILS=2` the container stops instead of running with a half-applied configuration. Missing
execute permission is not an error: a non-executable file is skipped, which makes it easy to keep a hook
`...sh.disabled` around.

Two rules keep hooks predictable:

- They run on every start, so write them to be repeatable.
- They do not restart services. Changing `main.cf` is fine because Postfix has not started yet; changing
  `opendkim.conf` is not.

## Derived images

Replacing a template is the blunt instrument: the whole rendered file becomes yours, and future jmail releases stop
applying to it.

```dockerfile
FROM ghcr.io/ffimnsr/jmail:latest
COPY main.cf.tmpl /etc/templates/postfix/main.cf.tmpl
```

`dockerize` is a full Go `text/template` with `env`, `default`, `exists` and `split` helpers, so a template can stay
configurable instead of hardcoded:

```
{{ if env "JMAIL_MAX_MESSAGE_SIZE" }}message_size_limit = {{ env "JMAIL_MAX_MESSAGE_SIZE" }}{{ else }}message_size_limit = 10240000{{ end }}
```

Note it is `env "NAME"`, not `getenv`.

Adding a service, or a oneshot that must run in a specific position, means adding an s6-rc service directory. The
shape matters: an s6 oneshot **`up` file is an execline script**, not a shell script. A `#!/usr/bin/with-contenv bash`
shebang there fails with `command exited 127`, or worse, is silently parsed as execline arguments. Point `up` at a
`run` script instead, exactly like the image's own `init-mailer-config` does:

```dockerfile
FROM ghcr.io/ffimnsr/jmail:latest

COPY hook.sh /etc/s6-overlay/s6-rc.d/init-custom/run
COPY init-custom.up /etc/s6-overlay/s6-rc.d/init-custom/up

RUN chmod 0755 /etc/s6-overlay/s6-rc.d/init-custom/run \
 && printf "oneshot\n" > /etc/s6-overlay/s6-rc.d/init-custom/type \
 && mkdir -p /etc/s6-overlay/s6-rc.d/init-custom/dependencies.d \
 && touch /etc/s6-overlay/s6-rc.d/init-custom/dependencies.d/init-mailer-config \
 && touch /etc/s6-overlay/user-bundles.d/user/contents.d/init-custom \
 && touch /etc/s6-overlay/s6-rc.d/svc-postfix/dependencies.d/init-custom
```

```
# /etc/s6-overlay/s6-rc.d/init-custom/up
/etc/s6-overlay/s6-rc.d/init-custom/run
```

```
# hook.sh, installed as .../init-custom/run
#!/usr/bin/with-contenv bash
set -e
postconf -e "smtpd_client_message_rate_limit = 60"
```

The first `touch` adds the service to the `user` bundle (s6-overlay 3.2.x keeps bundles under `user-bundles.d`, not
`s6-rc.d`), and the second makes `svc-postfix` wait for it. Without that second dependency the service runs, but
concurrently with Postfix starting, so your `postconf` change may lose the race.

## `/etc/cont-init.d` is not the place for this

s6-overlay 3.2.x still provides `legacy-cont-init`, so `/etc/cont-init.d` scripts do run. But that service is a
**sibling** of `init-mailer-config`, not a dependency of it, so a script there races the template render and its
`postconf` changes may be overwritten. Use `/etc/jmail/init.d` instead, or add a dependency as shown above.
