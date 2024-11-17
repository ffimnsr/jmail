FROM alpine:latest

ENV DOCKERIZE_VERSION=v0.7.0
ENV JMAIL_HOSTNAME=example.com
ENV JMAIL_ORIGIN=example.com
ENV JMAIL_ROOT_EMAIL=user@example.com
ENV JMAIL_SMTP_USER=jmail
ENV JMAIL_SMTP_PASSWORD=mysecretpassword
ENV S6_BEHAVIOUR_IF_STAGE2_FAILS=2

RUN apk add --no-cache \
  postfix opendkim opendkim-utils \
  ca-certificates tzdata bash \
  cyrus-sasl cyrus-sasl-crammd5 cyrus-sasl-digestmd5 cyrus-sasl-login cyrus-sasl-ntlm \
  mailx musl musl-utils s6-overlay

RUN wget -O - \
  https://github.com/jwilder/dockerize/releases/download/$DOCKERIZE_VERSION/dockerize-linux-amd64-$DOCKERIZE_VERSION.tar.gz | tar xzf - -C /usr/local/bin

COPY /data/s6-overlay /etc/s6-overlay
COPY /data/templates /etc/templates

VOLUME ["/var/spool/postfix", "/etc/postfix", "/etc/opendkim/keys"]

ENTRYPOINT ["/init"]

STOPSIGNAL SIGINT
EXPOSE 587
