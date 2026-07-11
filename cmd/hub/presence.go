// Copyright 2026 CiCy AI
// SPDX-License-Identifier: Apache-2.0

// Presence: the gateway's in-memory directory of every team's agents.
//
// A team node opens a WS to /_presence (authenticated by its typ=node JWT) and
// pushes its full agent list; the gateway records it in an in-memory map keyed
// by "<org>/<slug>". The map is the single most-complete view of all agents
// across all teams. When a team's report socket closes, its entry drops (its
// agents go offline). /_agents serves the aggregated snapshot.
package main

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/cicy-ai/cicy-tunnel/relay"
	"github.com/cicy-ai/cicy-tunnel/token"

	"github.com/coder/websocket"
)

// Agent is one agent's presence entry as reported by its team node — identity
// plus live telemetry (busy/idle, which model, how much context it's burning).
type Agent struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Type          string  `json:"type"`
	Role          string  `json:"role"`
	Status        string  `json:"status"`          // working(busy) | idle | completed | error | offline
	Model         string  `json:"model"`           // model currently in use
	ContextPct    float64 `json:"context_used_pct"` // % of the context window consumed
	ContextWindow int     `json:"context_window"`   // context window size (tokens)
	InputTokens   int64   `json:"input_tokens"`     // tokens on the current turn
	// Definition: the agent's guidance file (claude→CLAUDE.md, else AGENTS.md) and,
	// for cicy agents only, its role meta.yaml. This is the agent's full identity.
	DefFile string `json:"def_file"`           // "CLAUDE.md" | "AGENTS.md"
	Def     string `json:"def"`                // guidance file content
	MetaYAML string `json:"meta_yaml,omitempty"` // cicy roles only
}

// teamEntry is one team's live agent list in the registry. NO api_token: the hub
// never holds a node's token. It reaches nodes with its OWN hub token (which the
// node validates via JWKS), so each team's api_token stays on its own box and
// never enters the hub — no central token store to leak.
type teamEntry struct {
	Org      string    `json:"org"`
	Team     string    `json:"team"` // slug
	Agents   []Agent   `json:"agents"`
	LastSeen time.Time `json:"last_seen"`
}

// presEvent is a change broadcast to live /_client subscribers: a team's agent
// list changed ("upsert", carries the fresh list) or a team went offline.
type presEvent struct {
	Kind   string // "upsert" | "offline"
	Org    string
	Team   string
	Agents []Agent
}

// presence is the in-memory map: "<org>/<slug>" -> that team's agents. It also
// fans changes out to subscribed /_client sockets so mobile sees live upserts.
type presence struct {
	mu    sync.RWMutex
	teams map[string]*teamEntry
	subs  map[chan presEvent]struct{}
}

func newPresence() *presence {
	return &presence{teams: map[string]*teamEntry{}, subs: map[chan presEvent]struct{}{}}
}

// subscribe registers a listener for presence changes; call the returned func to
// unsubscribe. The channel is buffered — a slow client drops events (it will
// re-sync on the next periodic report) rather than blocking the reporter.
func (p *presence) subscribe() (chan presEvent, func()) {
	ch := make(chan presEvent, 64)
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}

// broadcast delivers an event to every subscriber without blocking (drop on full).
// Caller must hold no lock; broadcast takes the read lock over the subs set.
func (p *presence) broadcast(ev presEvent) {
	p.mu.RLock()
	for ch := range p.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	p.mu.RUnlock()
}

func (p *presence) set(org, team string, agents []Agent) {
	p.mu.Lock()
	p.teams[org+"/"+team] = &teamEntry{Org: org, Team: team, Agents: agents, LastSeen: time.Now()}
	p.mu.Unlock()
	p.broadcast(presEvent{Kind: "upsert", Org: org, Team: team, Agents: agents})
}

func (p *presence) drop(org, team string) {
	p.mu.Lock()
	delete(p.teams, org+"/"+team)
	p.mu.Unlock()
	p.broadcast(presEvent{Kind: "offline", Org: org, Team: team})
}

// orgOf returns the org that owns a team slug, or "" if unknown.
func (p *presence) orgOf(team string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, t := range p.teams {
		if t.Team == team {
			return t.Org
		}
	}
	return ""
}

// orgVisible reports whether a caller scoped to callerOrg may see teamOrg. An
// empty callerOrg is a self-host hub credential (no tenant boundary) and sees all.
func orgVisible(callerOrg, teamOrg string) bool {
	return callerOrg == "" || callerOrg == teamOrg
}

// snapshot returns every team's agents, teams sorted by key for stable output.
func (p *presence) snapshot() []teamEntry {
	p.mu.RLock()
	out := make([]teamEntry, 0, len(p.teams))
	for _, t := range p.teams {
		out = append(out, *t)
	}
	p.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Org+out[i].Team < out[j].Org+out[j].Team })
	return out
}

// presenceReportHandler: a node connects over WS (typ=node JWT) and pushes its
// agent list. Each message is the team's CURRENT full list: {"agents":[...]}.
// The gateway overwrites that team's entry in the map on every message; when the
// socket closes the team's agents drop offline.
func presenceReportHandler(pres *presence, pub *rsa.PublicKey) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cl, err := token.Verify(pub, relay.Bearer(r), token.TypNode)
		if err != nil {
			http.Error(w, "unauthorized node: "+err.Error(), http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() {
			pres.drop(cl.Org, cl.Subject)
			_ = c.Close(websocket.StatusNormalClosure, "")
		}()
		c.SetReadLimit(64 << 20) // reports carry full def files (CLAUDE.md/AGENTS.md/meta.yaml)
		ctx := context.Background()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var msg struct {
				Agents []Agent `json:"agents"`
			}
			if json.Unmarshal(data, &msg) == nil {
				pres.set(cl.Org, cl.Subject, msg.Agents)
			}
		}
	}
}

// agentJSON is the wire shape of one agent in the directory / upsert frames:
// identity + live telemetry + reach_url (the team's transparent-tunnel address).
// NO token: the client presents its OWN hub token to reach an agent; the node
// validates it (JWKS). The node's api_token is never disclosed.
func agentJSON(a Agent, reachURL string) map[string]interface{} {
	return map[string]interface{}{
		"wid": a.ID, "title": a.Title, "agent_type": a.Type, "role": a.Role,
		"status": a.Status, "model": a.Model,
		"context_used_pct": a.ContextPct, "context_window": a.ContextWindow,
		"reach_url": reachURL,
	}
}

// directory builds the aggregated agent directory shared by the /_agents HTTP
// snapshot and the /_client WS "directory" frame, scoped to callerOrg (empty =
// self-host, all teams).
func (p *presence) directory(hubDomain, callerOrg string) (teams []map[string]interface{}, total int) {
	snap := p.snapshot()
	teams = make([]map[string]interface{}, 0, len(snap))
	for _, t := range snap {
		if !orgVisible(callerOrg, t.Org) {
			continue
		}
		reach := "https://" + t.Team + "." + hubDomain
		ags := make([]map[string]interface{}, 0, len(t.Agents))
		for _, a := range t.Agents {
			ags = append(ags, agentJSON(a, reach))
			total++
		}
		teams = append(teams, map[string]interface{}{"team": t.Team, "org": t.Org, "agents": ags})
	}
	return teams, total
}

// presenceListHandler serves the full aggregated agent directory (all teams).
// Requires a typ=hub bearer (hubToken). This is the HTTP snapshot; the live
// mobile channel is the /_client WS (see docs).
func presenceListHandler(pres *presence, pub *rsa.PublicKey, hubDomain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cl, err := token.Verify(pub, relay.Bearer(r), token.TypHub)
		if err != nil {
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		teams, total := pres.directory(hubDomain, cl.Org)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"teams": teams, "team_count": len(teams), "agent_count": total,
		})
	}
}
