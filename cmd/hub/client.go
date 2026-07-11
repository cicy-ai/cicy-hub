// Copyright 2026 CiCy AI
// SPDX-License-Identifier: Apache-2.0

// The live mobile channel: /_client. A mobile app opens ONE WebSocket
// (authenticated by a typ=hub JWT) and drives everything over it — the agent
// directory, per-agent chat streams, history, and sending prompts. The hub is a
// transparent multiplexer: it relays each subscribed agent's node chat ws frames
// verbatim (wrapped in a {type:"chat"} envelope), and forwards history_req/send
// to the node over its live tunnel. See docs/mobile-integration.md.
package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/cicy-ai/cicy-tunnel/relay"
	"github.com/cicy-ai/cicy-tunnel/token"

	"github.com/coder/websocket"
)

// mobileClient is one connected /_client socket. All writes funnel through write()
// under wmu because the directory pump, N chat relays, and the read loop's acks
// all write concurrently. subs maps an agent address -> its chat relay's cancel.
type mobileClient struct {
	c     *websocket.Conn
	org   string // tenant scope; "" = self-host (sees all)
	wmu   sync.Mutex
	submu sync.Mutex
	subs  map[string]context.CancelFunc
}

func (m *mobileClient) write(ctx context.Context, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.wmu.Lock()
	defer m.wmu.Unlock()
	return m.c.Write(ctx, websocket.MessageText, b)
}

// clientHandler upgrades a hubToken-authenticated mobile socket, pushes the
// directory, streams presence upserts, and services subscribe/unsubscribe/
// history_req/send frames.
func clientHandler(pres *presence, reg *relay.Registry, pub *rsa.PublicKey, hubDomain string, webOrigins []string) http.HandlerFunc {
	// Browser WS clients send an Origin the JS can't remove; coder/websocket rejects
	// cross-origin by default (CSRF). Whitelist the hub's own host + localhost (dev)
	// + any configured web origins. Native clients send no Origin and are allowed.
	// Auth is still the hub token — the Origin check is only CSRF defense in depth.
	patterns := append([]string{"localhost:*", "127.0.0.1:*", "[::1]:*", hubDomain, "*." + hubDomain}, webOrigins...)
	acceptOpts := &websocket.AcceptOptions{OriginPatterns: patterns}
	return func(w http.ResponseWriter, r *http.Request) {
		cl, err := token.Verify(pub, relay.Bearer(r), token.TypHub)
		if err != nil {
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		callerOrg := cl.Org // "" = self-host hub credential (no tenant boundary)
		c, err := websocket.Accept(w, r, acceptOpts)
		if err != nil {
			return
		}
		c.SetReadLimit(8 << 20)
		ctx, cancel := context.WithCancel(context.Background())
		m := &mobileClient{c: c, org: callerOrg, subs: map[string]context.CancelFunc{}}
		defer func() {
			cancel()
			m.submu.Lock()
			for _, cf := range m.subs {
				cf()
			}
			m.submu.Unlock()
			_ = c.Close(websocket.StatusNormalClosure, "")
		}()

		// First frame: the directory snapshot, scoped to the caller's org.
		teams, total := pres.directory(hubDomain, callerOrg)
		_ = m.write(ctx, map[string]interface{}{
			"type": "directory", "teams": teams, "team_count": len(teams), "agent_count": total,
		})

		// Live presence: push agent_upsert / team_offline as teams change.
		evCh, unsub := pres.subscribe()
		defer unsub()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-evCh:
					if !ok {
						return
					}
					if !orgVisible(callerOrg, ev.Org) {
						continue // not this tenant's team
					}
					if ev.Kind == "offline" {
						_ = m.write(ctx, map[string]interface{}{"type": "team_offline", "team": ev.Team})
						continue
					}
					reach := "https://" + ev.Team + "." + hubDomain
					for _, a := range ev.Agents {
						_ = m.write(ctx, map[string]interface{}{
							"type": "agent_upsert", "team": ev.Team, "agent": agentJSON(a, reach),
						})
					}
				}
			}
		}()

		// Read loop: client → server frames.
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var f struct {
				Type   string `json:"type"`
				Agent  string `json:"agent"`
				Text   string `json:"text"`
				Submit bool   `json:"submit"`
				ReqID  string `json:"req_id"`
				Limit  int    `json:"limit"`
			}
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			switch f.Type {
			case "subscribe":
				m.subscribe(ctx, pres, reg, f.Agent)
			case "unsubscribe":
				m.unsubscribe(f.Agent)
			case "history_req":
				go m.history(ctx, pres, reg, f.ReqID, f.Agent, f.Limit)
			case "send":
				go m.send(ctx, pres, reg, f.ReqID, f.Agent, f.Text, f.Submit)
			}
		}
	}
}

// resolve splits "<team>.<wid>", enforces the caller's org boundary, and opens a
// yamux stream to that team's live tunnel. NO token is returned or forwarded: the
// node's own tunnel dialer injects the local api_token, so requests reach cicy-code
// authenticated without the token ever leaving the box. A caller scoped to an org
// can never resolve another tenant's team.
func (m *mobileClient) resolve(pres *presence, reg *relay.Registry, addr string) (wid string, dial func(context.Context) (net.Conn, error), ok bool) {
	team, wid, cut := strings.Cut(addr, ".")
	if !cut || team == "" || wid == "" {
		return "", nil, false
	}
	if !orgVisible(m.org, pres.orgOf(team)) {
		return "", nil, false
	}
	sess := reg.GetByNode(team)
	if sess == nil {
		return "", nil, false
	}
	return wid, func(context.Context) (net.Conn, error) { return sess.Open() }, true
}

// yamuxHTTP is an http.Client that dials the resolved node over its tunnel.
func yamuxHTTP(dial func(context.Context) (net.Conn, error)) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
	}}
}

// subscribe dials the agent's node chat ws over the tunnel and relays every frame
// verbatim as {type:"chat", agent, frame:<node frame>}. Idempotent per agent.
func (m *mobileClient) subscribe(parent context.Context, pres *presence, reg *relay.Registry, addr string) {
	wid, dial, ok := m.resolve(pres, reg, addr)
	if !ok {
		_ = m.write(parent, map[string]interface{}{"type": "error", "agent": addr, "error": "no live team for " + addr})
		return
	}
	m.submu.Lock()
	if _, exists := m.subs[addr]; exists {
		m.submu.Unlock()
		return // already streaming
	}
	sctx, scancel := context.WithCancel(parent)
	m.subs[addr] = scancel
	m.submu.Unlock()

	go func() {
		defer func() {
			m.submu.Lock()
			delete(m.subs, addr)
			m.submu.Unlock()
			scancel()
		}()
		// No token in the URL — the node's tunnel dialer injects the local api_token.
		u := "ws://tunnel/api/chat/ws?master_agent_id=" + url.QueryEscape(wid) +
			"&client_id=" + url.QueryEscape("hub-"+addr)
		nc, _, err := websocket.Dial(sctx, u, &websocket.DialOptions{HTTPClient: yamuxHTTP(dial)})
		if err != nil {
			_ = m.write(parent, map[string]interface{}{"type": "error", "agent": addr, "error": "subscribe: " + err.Error()})
			return
		}
		defer nc.Close(websocket.StatusNormalClosure, "")
		nc.SetReadLimit(16 << 20)
		_ = m.write(parent, map[string]interface{}{"type": "ack", "agent": addr, "ok": true})
		for {
			_, frame, err := nc.Read(sctx)
			if err != nil {
				return
			}
			_ = m.write(parent, map[string]interface{}{"type": "chat", "agent": addr, "frame": json.RawMessage(frame)})
		}
	}()
}

func (m *mobileClient) unsubscribe(addr string) {
	m.submu.Lock()
	if cf := m.subs[addr]; cf != nil {
		cf()
	}
	m.submu.Unlock()
}

// history fetches the agent's current-history over the tunnel and returns it in a
// {type:"history"} frame. turns is the node's current-history payload verbatim.
func (m *mobileClient) history(ctx context.Context, pres *presence, reg *relay.Registry, reqID, addr string, limit int) {
	wid, dial, ok := m.resolve(pres, reg, addr)
	if !ok {
		_ = m.write(ctx, map[string]interface{}{"type": "error", "req_id": reqID, "agent": addr, "error": "no live team"})
		return
	}
	u := "http://tunnel/api/agents/current-history/" + url.PathEscape(wid)
	if limit > 0 {
		u += "?limit=" + url.QueryEscape(itoa(limit))
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := yamuxHTTP(dial).Do(req)
	if err != nil {
		_ = m.write(ctx, map[string]interface{}{"type": "error", "req_id": reqID, "agent": addr, "error": "history: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	_ = m.write(ctx, map[string]interface{}{
		"type": "history", "req_id": reqID, "agent": addr, "turns": json.RawMessage(body),
	})
}

// send forwards a prompt to the agent's node /api/tmux/send over the tunnel.
func (m *mobileClient) send(ctx context.Context, pres *presence, reg *relay.Registry, reqID, addr, text string, submit bool) {
	wid, dial, ok := m.resolve(pres, reg, addr)
	if !ok {
		_ = m.write(ctx, map[string]interface{}{"type": "error", "req_id": reqID, "agent": addr, "error": "no live team"})
		return
	}
	payload, _ := json.Marshal(map[string]interface{}{"pane_id": wid, "text": text, "submit": submit})
	u := "http://tunnel/api/tmux/send"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := yamuxHTTP(dial).Do(req)
	if err != nil {
		_ = m.write(ctx, map[string]interface{}{"type": "error", "req_id": reqID, "agent": addr, "error": "send: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	_ = m.write(ctx, map[string]interface{}{"type": "ack", "req_id": reqID, "agent": addr, "ok": resp.StatusCode < 300, "node_status": resp.StatusCode})
}

// itoa avoids pulling strconv just for one int->string.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
