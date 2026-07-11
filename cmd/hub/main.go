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
	"os"

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
	pres := newPresence()
	rl.Mux.HandleFunc("/_presence", presenceReportHandler(pres, rl.Pub))
	rl.Mux.HandleFunc("/_agents", presenceListHandler(pres, rl.Pub, hubDomain))
	rl.Mux.HandleFunc("/_client", clientHandler(pres, rl.Reg, rl.Pub, hubDomain))
	rl.Mux.HandleFunc("/_console", consoleHandler)
	rl.Mux.HandleFunc("/_msg", msgHandler(rl.Reg, rl.Pub))
	log.Printf("[hub] /_agents + /_client + /_console + /_msg — on the relay")

	log.Fatal(rl.ListenAndServe())
}
