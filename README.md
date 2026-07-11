# cicy-hub

**One small server that makes every machine you own reachable from anywhere** —
at `https://<name>.hub.<your-domain>/` — with a single access token, **no inbound
ports opened on your machines**, and each machine's real secret never leaving it.

It's self-hosted and self-contained: on first boot the container generates its own
signing key, its own TLS cert, and its own license. No cloud account, no control
plane, nothing else to run.

```
   phone / browser                    cicy-hub  (one server, port 443)              your machines (no inbound port)
  ┌────────────────┐   hub token     ┌──────────────────────────────────┐  dial OUT  ┌───────────────────────┐
  │ open           │ ──────────────▶ │  *.hub.example.com                │ ◀───────── │ mac13   → cicy-code   │
  │ mac13.hub...   │   ?token=<hub>  │  ① check token  ② route by name   │  wss+yamux │ win10   → cicy-code   │
  └────────────────┘                 └──────────────────────────────────┘            └───────────────────────┘
```

Each machine **dials out** to the hub and holds the connection open. When you open
`mac13.hub.example.com`, the hub checks your token and pipes you down that machine's
tunnel — exactly as if you were on its `localhost`.

### Why this is safe

- **No open ports.** Machines dial *out*; there is nothing inbound to scan or attack.
- **One token for everything.** You carry a short-lived **hub token**. Each machine's
  own long-lived secret (`api_token`) is swapped in *on the machine* and never travels
  the network — a stolen hub token expires and never exposes a machine.
- **Tenant isolation.** Tokens are scoped to an org; a token for one org can't see another's machines.

---

## What you need

1. A **domain** you control (e.g. `example.com`).
2. A **wildcard DNS record**: `*.hub.example.com` → your server's public IP.
3. A **server** with **port 443** free and **Docker** installed.

That's the whole manual checklist. Everything below is copy-paste.

---

## 1 · Start the hub — one command

```sh
DOMAIN=example.com docker compose up -d
```

This pulls the released image and serves `*.hub.example.com` on `:443` with a
self-signed cert (instant, great for testing — browsers show a "not private" warning
you can click through). Want a browser-trusted cert? See
[Trusted certificate](#trusted-certificate-lets-encrypt) below.

> Building from source instead of pulling? Add `--build`:
> `DOMAIN=example.com docker compose up -d --build`

**Check it's alive:**

```sh
curl -k https://hub.example.com/_gw/health
# {"status":"ok","source":"cicy-tunnel"}
```

---

## 2 · Connect a machine

On the hub, mint that machine a **node token** and get its dial URL:

```sh
docker compose exec hub enroll mac13
```

Then, **on the machine** (`mac13`, running cicy-code on `:8008`), run the small
dialer. It dials out with the node token and injects the machine's *local*
`api_token`, so the hub never needs it:

```sh
node \
  -gateway wss://mac13.hub.example.com/_tunnel/connect \
  -token   <node-token-from-enroll> \
  -local   127.0.0.1:8008 \
  -inject-token "$(sed -n 's/.*"api_token" *: *"\([^"]*\)".*/\1/p' ~/cicy-ai/global.json)" \
  -insecure          # only for a self-signed hub; drop it once you have a real cert
```

- `-inject-token` is the whole security trick: the caller presents a **hub** token;
  the dialer swaps in this machine's **local** api_token before handing the request to
  cicy-code — so the api_token stays on the box.
- The **`hub-tunnel` helper skill** ships this `node` binary and installs it as an
  auto-restart service (launchd / supervisor), so you don't run it by hand.

**Check the machine is up** — the hub logs `tunnel up self/mac13`:

```sh
docker compose logs --tail=20 hub | grep "tunnel up"
```

Repeat for every machine (`enroll win10`, `enroll server-1`, …).

---

## 3 · Get an access token

One **hub token** reaches every machine in your org:

```sh
docker compose exec hub grant 720h    # 30 days; omit for the default
```

Copy the printed token. (Lost it? Just `grant` another — they're independent.)

---

## 4 · Open a machine

**In a browser** — append the token once; the hub drops it into a cookie so the rest
of the page loads normally:

```
https://mac13.hub.example.com/?token=<hub-token>
```

The **same token works for every machine** — just change the name
(`win10.hub.example.com/?token=…`). It's checked for validity only, not tied to one machine.

**On your phone** — paste the hub URL + token into the app's *Add Hub*:

```
Hub:   https://hub.example.com
Token: <hub-token>
```

The app lists all your machines and their agents, live.

**Operator console** — a built-in web dashboard of every connected machine:

```
https://hub.example.com/_console?token=<hub-token>
```

---

## Trusted certificate (Let's Encrypt)

Wildcard certs are issued via a DNS challenge, so give the hub your DNS provider's
API token. Example with Cloudflare:

```sh
DOMAIN=example.com \
TLS_MODE=letsencrypt \
ACME_EMAIL=you@example.com \
LEGO_PROVIDER=cloudflare \
CF_DNS_API_TOKEN=xxxxxxxx \
docker compose up -d
```

For other providers set `LEGO_PROVIDER` and its env per
[lego's provider list](https://go-acme.github.io/lego/dns/). Now the self-signed
warning (and the node's `-insecure`) is gone.

### Bring your own certificate

```sh
# put your fullchain + key in the volume as /data/tls-cert.pem and /data/tls-key.pem, then:
DOMAIN=example.com TLS_MODE=custom docker compose up -d
```

---

## Settings

All optional except `DOMAIN`. Set them inline (`FOO=bar docker compose up -d`) or in a `.env` file.

| env | default | what it does |
|---|---|---|
| `DOMAIN` | **required** | serves `*.hub.$DOMAIN` |
| `ORG` | `self` | groups the machines you own (tenant scope) |
| `TLS_MODE` | `selfsigned` | `selfsigned` · `letsencrypt` · `custom` |
| `LICENSE_MAX_NODES` | `50` | how many machines may connect at once |
| `NODE_TTL` | `8760h` | how long a minted node token stays valid (~1 year) |
| `HUB_WEB_ORIGINS` | — | extra browser origins allowed to call the hub (comma-separated; only if you host the web UI on a *different* domain) |
| `ADDR` | `:443` | listen address |
| `CICY_HUB_TAG` | `latest` | which released image tag to run |

Handy commands:

```sh
docker compose exec hub info          # print current config (domain, org, tls, pubkey)
docker compose exec hub enroll <name> # add a machine (node token + dial URL)
docker compose exec hub grant [ttl]   # mint an access (hub) token
```

---

## Endpoints (reference)

| path | who calls it | auth |
|---|---|---|
| `https://<name>.hub.$DOMAIN/…` | you (browser / app / API) | **hub token** — gated, then piped to that machine |
| `/_gw/health` | anyone | none (liveness) |
| `/_tunnel/connect` | a machine's dialer | **node token** |
| `/_agents` | app / dashboard | **hub token** — JSON directory of all machines + agents |
| `/_client` | mobile app (WebSocket) | **hub token** — live directory + chat + send |
| `/_console` | browser | **hub token** — the operator dashboard |
| `/_presence` | a machine's reporter | **node token** — pushes that machine's agent list |
| `/_msg` | a machine (agent) | **node token** — cross-machine message routing |

See [`docs/mobile-integration.md`](docs/mobile-integration.md) for the `/_agents` and
`/_client` payload shapes (building an app against the hub).

---

## The three tokens

The hub is its own token authority — it mints and verifies all of these with the
keypair it generated on first boot.

- **node token** (`enroll`) — proves a machine may dial in and claim a name. Long-lived, lives on the machine.
- **hub token** (`grant`) — proves *you* may reach machines. This is what you carry. Short-lived, safe to hand out.
- **license** — self-issued on boot; just lifts the "1 machine" cap to `LICENSE_MAX_NODES`.

An `api_token` is **not** on this list — that's each machine's own cicy-code secret,
and the hub never holds it.

---

## Operating notes

- **Identity persists** in the `gwdata` Docker volume (signing key, cert, license).
  **Keep it** — regenerating the signing key invalidates every node/hub token you've minted.
- Update: `CICY_HUB_TAG=<new> docker compose pull && docker compose up -d`.
- The relay verifies only a machine's *dial* and a caller's *token*; it never inspects
  or rewrites the traffic it pipes.

---

## Releasing

Cutting a release is one tag — CI builds and publishes the image:

```sh
git tag v1.0.0
git push origin v1.0.0
```

The [release workflow](.github/workflows/release.yml) builds and pushes
`ghcr.io/cicy-ai/cicy-hub:1.0.0`, `:1.0`, and `:latest`. Then on any server:

```sh
CICY_HUB_TAG=1.0.0 DOMAIN=example.com docker compose up -d
```

Manual (no CI):

```sh
docker build -t ghcr.io/cicy-ai/cicy-hub:1.0.0 .
docker push  ghcr.io/cicy-ai/cicy-hub:1.0.0
```

---

## Building from source

```sh
go build ./cmd/hub        # the hub
go build ./cmd/reporter   # the presence reporter a machine runs
```

Needs Go 1.23+ and the `github.com/cicy-ai/cicy-tunnel` module (the relay core, the
`node` dialer, and the `mint` token tool live there).
