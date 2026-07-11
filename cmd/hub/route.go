// Copyright 2026 CiCy AI
// SPDX-License-Identifier: Apache-2.0

// Cross-team message routing. POST /_msg addresses an agent as <team>.<agent>
// (e.g. teamA.1001); the hub resolves the target team's live tunnel and delivers
// the message to that agent's cicy-code /api/tmux/send, stamping the message with
// the team-qualified sender [team.agent] so the recipient can reply back through
// the hub. This is the routing layer on top of presence.
package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/cicy-ai/cicy-tunnel/relay"
	"github.com/cicy-ai/cicy-tunnel/token"
)

func msgHandler(reg *relay.Registry, pub *rsa.PublicKey) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// The caller is a team node (typ=node JWT) — identifies the SENDER's team.
		cl, err := token.Verify(pub, relay.Bearer(r), token.TypNode)
		if err != nil {
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		var req struct {
			To        string `json:"to"`         // <team>.<agent>, e.g. teamA.1001
			Text      string `json:"text"`
			Token     string `json:"token"`      // target node's api_token (sender-supplied for now)
			FromAgent string `json:"from_agent"` // sender agent id, for the [team.agent] stamp
			Submit    bool   `json:"submit"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.To == "" || req.Token == "" {
			http.Error(w, "need {to:<team>.<agent>, text, token}", http.StatusBadRequest)
			return
		}
		team, agent, ok := strings.Cut(req.To, ".")
		if !ok || team == "" || agent == "" {
			http.Error(w, "bad address; want <team>.<agent> (e.g. teamA.1001)", http.StatusBadRequest)
			return
		}
		sess := reg.GetByNode(team)
		if sess == nil {
			http.Error(w, "no live team "+team, http.StatusBadGateway)
			return
		}
		pane := agent
		if !strings.HasPrefix(pane, "w-") {
			pane = "w-" + pane
		}
		from := cl.Subject
		if req.FromAgent != "" {
			from = cl.Subject + "." + req.FromAgent
		}
		stamp := "📮 [" + from + "] " + req.Text

		// Deliver over the target team's tunnel to its cicy-code /api/tmux/send.
		body, _ := json.Marshal(map[string]interface{}{"pane_id": pane, "text": stamp, "submit": req.Submit})
		up, _ := http.NewRequest(http.MethodPost, "http://tunnel/api/tmux/send?token="+req.Token, bytes.NewReader(body))
		up.Header.Set("Content-Type", "application/json")
		hc := &http.Client{Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return sess.Open() },
		}}
		resp, err := hc.Do(up)
		if err != nil {
			http.Error(w, "deliver failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"routed_to":   req.To,
			"pane":        pane,
			"from":        from,
			"node_status": resp.StatusCode,
			"node_reply":  json.RawMessage(rb),
		})
	}
}
