#!/bin/sh
# cicy-hub container entrypoint — auto-configures everything on first boot:
#   1. signing keypair   (jwt-key.pem / jwt-pub.pem)   — "who may dial in"
#   2. TLS cert          (tls-cert.pem / tls-key.pem)  — self-signed | letsencrypt | custom
#   3. wildcard license  (license.jwt)                 — lifts the 1-node cap
# then starts the relay. No cicy-cloud, no external control plane required.
#
# Subcommands:
#   serve            (default) auto-config + run the gateway
#   enroll <slug>    mint a node token + print the CICY_GATEWAY_URL/TOKEN to paste on a node
#   info             print the gateway's public config
set -eu

DATA=/data
DOMAIN="${DOMAIN:-}"
ADDR="${ADDR:-:443}"
ORG="${ORG:-self}"
TLS_MODE="${TLS_MODE:-selfsigned}"
LICENSE_MAX_NODES="${LICENSE_MAX_NODES:-50}"
NODE_TTL="${NODE_TTL:-8760h}"          # ~1 year
KEY="$DATA/jwt-key.pem"
PUB="$DATA/jwt-pub.pem"
CRT="$DATA/tls-cert.pem"
CRTKEY="$DATA/tls-key.pem"
LIC="$DATA/license.jwt"

need_domain() {
  if [ -z "$DOMAIN" ]; then
    echo "ERROR: DOMAIN is required (e.g. DOMAIN=example.com → serves *.hub.example.com)" >&2
    exit 2
  fi
}

ensure_keypair() {
  if [ ! -f "$KEY" ] || [ ! -f "$PUB" ]; then
    echo "[entrypoint] generating node-admittance signing keypair"
    ( cd "$DATA" && mint keygen >/dev/null )   # writes jwt-key.pem + jwt-pub.pem in CWD
  fi
}

ensure_license() {
  # A self-issued wildcard license (org="") simply sets the node cap on YOUR OWN
  # gateway. Without it the relay caps 1 node; a self-host mesh wants several.
  if [ ! -f "$LIC" ]; then
    echo "[entrypoint] issuing wildcard license (max_nodes=$LICENSE_MAX_NODES)"
    mint license -org "" -max-nodes "$LICENSE_MAX_NODES" -priv "$KEY" > "$LIC"
  fi
}

ensure_tls() {
  case "$TLS_MODE" in
    custom)
      if [ ! -f "$CRT" ] || [ ! -f "$CRTKEY" ]; then
        echo "ERROR: TLS_MODE=custom needs $CRT + $CRTKEY mounted into /data" >&2
        exit 2
      fi
      echo "[entrypoint] TLS: using mounted cert"
      ;;
    letsencrypt)
      : "${ACME_EMAIL:?TLS_MODE=letsencrypt needs ACME_EMAIL}"
      : "${LEGO_PROVIDER:?TLS_MODE=letsencrypt needs LEGO_PROVIDER (e.g. cloudflare) + its DNS API env}"
      wc="_.hub.$DOMAIN"
      if [ ! -f "$DATA/lego/certificates/$wc.crt" ]; then
        echo "[entrypoint] TLS: requesting Let's Encrypt wildcard *.hub.$DOMAIN via DNS-01 ($LEGO_PROVIDER)"
        lego --accept-tos --email "$ACME_EMAIL" --dns "$LEGO_PROVIDER" \
             --domains "*.hub.$DOMAIN" --domains "hub.$DOMAIN" --path "$DATA/lego" run
      fi
      cp "$DATA/lego/certificates/$wc.crt" "$CRT"
      cp "$DATA/lego/certificates/$wc.key" "$CRTKEY"
      ;;
    selfsigned|*)
      if [ ! -f "$CRT" ] || [ ! -f "$CRTKEY" ]; then
        echo "[entrypoint] TLS: self-signed for *.hub.$DOMAIN (browsers will warn; fine for testing)"
        openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
          -keyout "$CRTKEY" -out "$CRT" \
          -subj "/CN=cicy-hub/O=$DOMAIN" \
          -addext "subjectAltName=DNS:*.hub.$DOMAIN,DNS:hub.$DOMAIN,DNS:$DOMAIN,DNS:localhost,IP:127.0.0.1" 2>/dev/null
      fi
      ;;
  esac
}

cmd="${1:-serve}"
case "$cmd" in
  serve)
    need_domain
    ensure_keypair
    ensure_license
    ensure_tls
    # The hub binary reads these from the env: the agent directory builds each
    # team's reach_url as https://<slug>.$HUB_DOMAIN, and the web UI / mobile WS
    # allow these extra browser origins (comma-separated) on top of same-origin.
    export HUB_DOMAIN="hub.$DOMAIN"
    export HUB_WEB_ORIGINS="${HUB_WEB_ORIGINS:-}"
    echo "[entrypoint] starting gateway on $ADDR — nodes dial wss://<slug>.hub.$DOMAIN/_tunnel/connect"
    exec hub -addr "$ADDR" -cert "$CRT" -key "$CRTKEY" -pub "$PUB" -license "$LIC"
    ;;
  enroll)
    need_domain
    ensure_keypair
    slug="${2:-}"
    [ -n "$slug" ] || { echo "usage: enroll <slug> [ttl]" >&2; exit 2; }
    ttl="${3:-$NODE_TTL}"
    tok="$(mint node -org "$ORG" -id "$slug" -ttl "$ttl" -priv "$KEY")"
    url="wss://$slug.hub.$DOMAIN/_tunnel/connect"
    ins=""
    [ "$TLS_MODE" = "selfsigned" ] && ins=" -insecure"   # self-signed → dialer must skip TLS verify
    echo "# On '$slug' (running cicy-code on :8008) install the dialer once:"
    echo "#   go install github.com/cicy-ai/cicy-tunnel/cmd/node@latest"
    echo "#   mv \"\$(go env GOPATH)/bin/node\" /usr/local/bin/cicy-node"
    echo "# then run this (it injects THIS box's local api_token; run it under a supervisor):"
    echo "cicy-node -gateway $url -token $tok -local 127.0.0.1:8008$ins \\"
    echo "  -inject-token \"\$(sed -n 's/.*\"api_token\" *: *\"\\([^\"]*\\)\".*/\\1/p' ~/cicy-ai/global.json)\""
    ;;
  grant)
    # Mint a hub token (typ=hub) with THIS hub's own private key — the access
    # credential a client presents to reach agents. The hub gates every request
    # with it; the node's dialer swaps it for the local api_token. Self-signed
    # and self-verified: no external issuer.
    need_domain
    ensure_keypair
    ttl="${2:-720h}"     # 30 days — this is the credential you hand out; keep it short-ish
    tok="$(mint hub -org "$ORG" -ttl "$ttl" -priv "$KEY")"
    echo "# hub token — org=$ORG, ttl=$ttl. A client reaches any team in this org with it:"
    echo "$tok"
    echo
    echo "#   https://<slug>.hub.$DOMAIN/?token=<token>"
    echo "#   or  Authorization: Bearer <token>"
    ;;
  info)
    need_domain
    ensure_keypair
    echo "domain:     *.hub.$DOMAIN"
    echo "org:        $ORG"
    echo "tls_mode:   $TLS_MODE"
    echo "pub key:    $PUB"
    echo "add a team:      docker compose exec hub enroll <slug>"
    echo "grant access:    docker compose exec hub grant [ttl]"
    ;;
  *)
    exec "$@"
    ;;
esac
