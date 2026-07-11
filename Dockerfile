# cicy-hub — self-contained, self-configuring zero-trust reverse-tunnel relay.
#
# Multi-stage: build the gateway + the `mint` token tool, then a tiny runtime that
# auto-generates its own signing key + TLS cert on first boot (no cicy-cloud needed).
# See README.md for the one-command quickstart.

# ── build ──────────────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hub ./cmd/hub \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mint    github.com/cicy-ai/cicy-tunnel/cmd/mint

# lego: single-binary ACME client for optional Let's Encrypt (DNS-01 wildcard).
# Only used when TLS_MODE=letsencrypt; harmless otherwise.
FROM alpine:3.20 AS lego
ARG LEGO_VERSION=4.17.4
RUN apk add --no-cache curl tar \
 && curl -fsSL "https://github.com/go-acme/lego/releases/download/v${LEGO_VERSION}/lego_v${LEGO_VERSION}_linux_amd64.tar.gz" \
      | tar -xz -C /usr/local/bin lego \
 && chmod +x /usr/local/bin/lego

# ── runtime ────────────────────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates openssl tini
COPY --from=build /out/hub /usr/local/bin/hub
COPY --from=build /out/mint    /usr/local/bin/mint
COPY --from=lego  /usr/local/bin/lego /usr/local/bin/lego
COPY docker-entrypoint.sh /usr/local/bin/entrypoint
RUN chmod +x /usr/local/bin/entrypoint

# Persist keys + certs here (mount a volume) so restarts keep the same identity.
VOLUME /data
WORKDIR /data
EXPOSE 443
ENTRYPOINT ["/sbin/tini","--","/usr/local/bin/entrypoint"]
CMD ["serve"]
