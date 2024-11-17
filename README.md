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
HealthCmd=["CMD", "nc -vz localhost 25"]
HealthInterval=30s
HealthRetries=5
HealthStartPeriod=20s
HealthTimeout=3s
PublishPort=10587:587

[Service]
Restart=always

[Install]
WantedBy=multi-user.target default.target
```

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


## License
jmail is licensed under the Apache-2.0 License. See [LICENSE](LICENSE) file for more details.
