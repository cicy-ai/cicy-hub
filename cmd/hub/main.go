// Copyright 2026 CiCy AI
// SPDX-License-Identifier: Apache-2.0

// cicy-hub: the agent collaboration hub. It EMBEDS cicy-tunnel's relay (so it is
// reachable + transparently proxies exactly like the standalone tunnel) and adds,
// on the SAME server, the agent directory (/_agents, fed over /_presence) and
// cross-team message routing (/_msg). One process, no extra hop — the routing
// reuses the relay's live session Registry.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/cicy-ai/cicy-tunnel/relay"
	"github.com/cicy-ai/cicy-tunnel/token"
)

func main() {
	cfg := relay.Config{}
	flag.StringVar(&cfg.Addr, "addr", ":7443", "HTTPS listen address")
	flag.StringVar(&cfg.Cert, "cert", "tls-cert.pem", "TLS cert pem")
	flag.StringVar(&cfg.Key, "key", "tls-key.pem", "TLS key pem")
	flag.StringVar(&cfg.PubPath, "pub", "jwt-pub.pem", "JWT verify public key file (used if -jwks-url empty)")
	flag.StringVar(&cfg.JWKSURL, "jwks-url", "", "fetch the JWT verify key from a JWKS endpoint")
	flag.StringVar(&cfg.LicensePath, "license", "", "path to a license JWT (unlocks the org's node cap)")
	flag.Parse()

	rl, err := relay.New(cfg)
	if err != nil {
		log.Fatalf("relay: %v", err)
	}

	// Gate the transparent proxy: a client reaching <slug>.<host> must present a
	// hub token (typ=hub). The node's tunnel dialer injects the local api_token, so
	// reach is authenticated at the hub and the api_token never leaves the box.
	rl.Authorize = func(r *http.Request) bool {
		_, err := token.Verify(rl.Pub, relay.Bearer(r), token.TypHub)
		return err == nil
	}

	// The agent layer, mounted on the relay's own mux (specific patterns beat "/").
	// reach_url = https://<team>.<hubDomain>. Set HUB_DOMAIN to your hub's domain
	// (e.g. hub.example.com); defaults to localhost for a local/dev run.
	hubDomain := os.Getenv("HUB_DOMAIN")
	if hubDomain == "" {
		hubDomain = "localhost"
	}
	// Extra browser origins allowed on the /_client WS (comma-separated), e.g. a
	// web app's dev server or a separate web domain. Same-origin + localhost are
	// always allowed; HUB_WEB_ORIGINS adds to that.
	var webOrigins []string
	for _, o := range strings.Split(os.Getenv("HUB_WEB_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			webOrigins = append(webOrigins, o)
		}
	}
	// CORS: a browser SPA reaching <slug>.<hubDomain>/api/... from another origin
	// (web dev server, a separate web domain) needs the hub to answer preflight and
	// stamp ACAO. Same trust set as the /_client WS: hub domain + localhost + any
	// HUB_WEB_ORIGINS. Auth is still the hub token; CORS only unblocks the browser.
	rl.AllowOrigin = originAllower(hubDomain, webOrigins)

	pres := newPresence()
	rl.Mux.HandleFunc("/_presence", presenceReportHandler(pres, rl.Pub))
	rl.Mux.HandleFunc("/_agents", presenceListHandler(pres, rl.Pub, hubDomain))
	rl.Mux.HandleFunc("/_client", clientHandler(pres, rl.Reg, rl.Pub, hubDomain, webOrigins))
	rl.Mux.HandleFunc("/_console", consoleHandler)
	rl.Mux.HandleFunc("/_msg", msgHandler(rl.Reg, rl.Pub))
	log.Printf("[hub] /_agents + /_client + /_console + /_msg — on the relay")

	log.Fatal(rl.ListenAndServe())
}

// originAllower builds the CORS Origin predicate: the hub's own domain and every
// slug subdomain (*.hubDomain), localhost/loopback (dev), plus any explicit
// HUB_WEB_ORIGINS. Each allowed entry is reduced to a hostname so an Origin like
// "http://localhost:8081" or "https://app.example.com" matches regardless of port.
func originAllower(hubDomain string, webOrigins []string) func(string) bool {
	allow := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, hubDomain: true}
	for _, o := range webOrigins {
		allow[hostOf(o)] = true
	}
	suffix := "." + hubDomain // slug subdomains: mac13.hub.example.com
	return func(origin string) bool {
		host := hostOf(origin)
		if host == "" {
			return false
		}
		return allow[host] || strings.HasSuffix(host, suffix)
	}
}

// hostOf reduces an origin/host pattern ("http://h:port", "h:port", "h") to its
// bare hostname, lowercased. Returns "" if nothing usable.
func hostOf(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Hostname() != "" {
			return strings.ToLower(u.Hostname())
		}
	}
	if u, err := url.Parse("http://" + s); err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	return strings.ToLower(s)
}
