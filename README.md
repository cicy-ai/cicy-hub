# cicy-hub — self-hosted, one-command

A neutral zero-trust **reverse-tunnel relay**. Nodes dial OUT to it (no inbound
port on the node); anyone can reach a node at `https://<slug>.hub.<your-domain>/`
exactly as if it were that node's `localhost`. The relay is **transparent** — it
forwards traffic byte-for-byte and never authenticates or inspects it. Auth is
the node's own job (its api_token), same as local.

This image is fully standalone: on first boot it generates its **own signing key**
(deciding which nodes may dial in), its **own TLS cert**, and a **license** — no
cicy-cloud, no external control plane.

## What you need (the only manual bits)

1. **A domain** you control.
2. **A wildcard DNS record**: `*.hub.<your-domain>` → this server's public IP.
3. **A server** with port **443** free.

Everything else is automatic.

## Quickstart (self-signed — instant, for testing)

```sh
DOMAIN=example.com docker compose up -d --build
```

That's it. The relay is live on `:443` serving `*.hub.example.com`.
(Self-signed → browsers show a warning. For a trusted cert see below.)

### Point a node at it

```sh
docker compose exec hub enroll my-mac
# → CICY_GATEWAY_URL=wss://my-mac.hub.example.com/_tunnel/connect
#   CICY_GATEWAY_TOKEN=<token>
```

Paste those into the node (env vars, or `~/cicy-ai/db/gateway.json`) and start
cicy-code with `--public`. Then open:

```
https://my-mac.hub.example.com/?token=<that node's api_token>
```

Same experience as `localhost:8008` — the node's api_token gates every request.

## Browser-trusted cert (Let's Encrypt, automatic)

Wildcard certs need a DNS-01 challenge, so give the relay your DNS provider's API
token. Example with Cloudflare:

```sh
DOMAIN=example.com \
TLS_MODE=letsencrypt \
ACME_EMAIL=you@example.com \
LEGO_PROVIDER=cloudflare \
CF_DNS_API_TOKEN=xxxxxxxx \
docker compose up -d --build
```

(For other providers, set `LEGO_PROVIDER` and its env per
[lego's provider list](https://go-acme.github.io/lego/dns/).)

## Bring your own cert

```sh
# drop fullchain + key into the volume, then:
DOMAIN=example.com TLS_MODE=custom docker compose up -d --build
#   /data/tls-cert.pem  /data/tls-key.pem
```

## Knobs

| env | default | meaning |
|---|---|---|
| `DOMAIN` | — (required) | serves `*.hub.$DOMAIN` |
| `ORG` | `self` | logical owner grouping your nodes |
| `LICENSE_MAX_NODES` | `50` | concurrent node cap on your relay |
| `TLS_MODE` | `selfsigned` | `selfsigned` \| `letsencrypt` \| `custom` |
| `NODE_TTL` | `8760h` | validity of a minted node token |
| `ADDR` | `:443` | listen address |

## Notes

- **Identity persists** in the `gwdata` volume (signing key, cert, license). Keep
  it — regenerating the key invalidates every node token you've minted.
- The relay verifies only the node's **dial** (its signed node token), to learn
  which slug maps to which live tunnel. Client traffic is never inspected.
- `docker compose exec hub info` prints the current config.
