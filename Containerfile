FROM alpine:latest

LABEL org.opencontainers.image.title jmail
LABEL org.opencontainers.image.description Send-only SMTP mail server
LABEL org.opencontainers.image.licenses Apache-2.0
LABEL org.opencontainers.image.url https://github.com/ffimnsr/jmail
LABEL org.opencontainers.image.source https://github.com/ffimnsr/jmail
LABEL org.opencontainers.image.documentation https://github.com/ffimnsr/jmail/README.md
LABEL org.opencontainers.image.base.name docker.io/library/alpine:latest
LABEL org.opencontainers.image.version <version>
LABEL org.opencontainers.image.revision <revision>

ENV DOCKERIZE_VERSION v0.7.0
ENV JMAIL_HOSTNAME example.com
ENV JMAIL_ORIGIN example.com
ENV JMAIL_ROOT_EMAIL user@example.com
ENV S6_BEHAVIOUR_IF_STAGE2_FAILS 2

RUN apk add --no-cache \
  postfix opendkim opendkim-utils \
  ca-certificates tzdata bash \
  musl musl-utils s6-overlay

RUN wget -O - \
  https://github.com/jwilder/dockerize/releases/download/$DOCKERIZE_VERSION/dockerize-linux-amd64-$DOCKERIZE_VERSION.tar.gz | tar xzf - -C /usr/local/bin

COPY /data/s6-overlay /etc/s6-overlay
COPY /data/templates /etc/templates

VOLUME ["/var/spool/postfix", "/etc/postfix", "/etc/opendkim/keys"]

ENTRYPOINT ["/init"]

STOPSIGNAL SIGINT
EXPOSE 587
