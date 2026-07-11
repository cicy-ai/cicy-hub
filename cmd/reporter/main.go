// Copyright 2026 CiCy AI
// SPDX-License-Identifier: Apache-2.0

// reporter is a demo stand-in for what a cicy-code node will do natively: read
// its local agent list (/api/panes) and PUSH it over a WS to the gateway's
// /_presence, so the gateway's in-memory directory always has this team's agents.
//
//	reporter -gateway wss://gw/_presence -token-file node.jwt \
//	         -local http://127.0.0.1:8208 -api-token <t> -insecure
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type agent struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Type          string  `json:"type"`
	Role          string  `json:"role"`
	Status        string  `json:"status"`
	Model         string  `json:"model"`
	ContextPct    float64 `json:"context_used_pct"`
	ContextWindow int     `json:"context_window"`
	InputTokens   int64   `json:"input_tokens"`
	DefFile       string  `json:"def_file"`
	Def           string  `json:"def"`
	MetaYAML      string  `json:"meta_yaml,omitempty"`
}

func main() {
	gw := flag.String("gateway", "", "gateway presence WS url (wss://<gw>/_presence)")
	tokFile := flag.String("token-file", "", "node JWT file (typ=node)")
	local := flag.String("local", "http://127.0.0.1:8008", "local cicy-code base url")
	apiToken := flag.String("api-token", "", "cicy-code api_token to read /api/panes")
	insecure := flag.Bool("insecure", false, "skip TLS verify")
	every := flag.Duration("every", 5*time.Second, "re-report interval")
	flag.Parse()

	tokBytes, err := os.ReadFile(*tokFile)
	if err != nil {
		log.Fatalf("read token: %v", err)
	}
	nodeTok := strings.TrimSpace(string(tokBytes))

	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure}}}
	c, _, err := websocket.Dial(context.Background(), *gw, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + nodeTok}},
		HTTPClient: hc,
	})
	if err != nil {
		log.Fatalf("dial presence: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	log.Printf("[reporter] presence up → %s (reading %s)", *gw, *local)

	for {
		agents := readAgents(hc, *local, *apiToken)
		// Report ONLY the agent list — never the api_token. The hub reaches nodes
		// with its OWN hub token (JWKS-verified by the node), so the node's api_token
		// stays local and never enters the hub. (api_token is used here only to read
		// /api/panes locally.)
		payload, _ := json.Marshal(map[string]interface{}{"agents": agents})
		if err := c.Write(context.Background(), websocket.MessageText, payload); err != nil {
			log.Fatalf("[reporter] write: %v", err)
		}
		log.Printf("[reporter] reported %d agents", len(agents))
		time.Sleep(*every)
	}
}

func readAgents(hc *http.Client, base, apiToken string) []agent {
	u := strings.TrimRight(base, "/") + "/api/panes?token=" + url.QueryEscape(apiToken)
	resp, err := hc.Get(u)
	if err != nil {
		log.Printf("[reporter] read panes: %v", err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var doc struct {
		Panes []struct {
			PaneID    string `json:"pane_id"`
			ID        string `json:"id"`
			Title     string `json:"title"`
			AgentType string `json:"agent_type"`
			Role      string `json:"role"`
			Workspace string `json:"workspace"`
		} `json:"panes"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	out := make([]agent, 0, len(doc.Panes))
	for _, p := range doc.Panes {
		id := p.PaneID
		if id == "" {
			id = p.ID
		}
		a := agent{ID: id, Title: p.Title, Type: p.AgentType, Role: p.Role, Status: "idle"}
		// Live telemetry per agent: busy/idle, model, context %, tokens.
		enrichStatus(hc, base, apiToken, id, &a)
		// Definition files: guidance (claude→CLAUDE.md, else AGENTS.md) + meta.yaml (cicy only).
		enrichDef(p.Workspace, p.AgentType, p.Title, &a)
		out = append(out, a)
	}
	return out
}

// enrichDef reads the agent's guidance file from its workspace and, for cicy
// agents, its role meta.yaml (from <cicyRoot>/memory/agents/<slug>/meta.yaml).
func enrichDef(workspace, atype, title string, a *agent) {
	if workspace == "" {
		return
	}
	a.DefFile = "AGENTS.md"
	if atype == "claude" {
		a.DefFile = "CLAUDE.md"
	}
	if b, err := os.ReadFile(filepath.Join(workspace, a.DefFile)); err == nil {
		a.Def = string(b)
	}
	if atype != "cicy" {
		return // non-cicy: no meta.yaml
	}
	// cicyRoot = <...>/cicy-ai (workspace is <cicyRoot>/workers/<pane>).
	cicyRoot := filepath.Dir(filepath.Dir(workspace))
	slug := roleSlug(title)
	if b, err := os.ReadFile(filepath.Join(cicyRoot, "memory", "agents", slug, "meta.yaml")); err == nil {
		a.MetaYAML = string(b)
	}
}

// roleSlug maps a cicy agent title to its role directory slug.
func roleSlug(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// enrichStatus fills the agent's live status/model/context from cicy-code's
// per-agent current-reply endpoint.
func enrichStatus(hc *http.Client, base, apiToken, paneID string, a *agent) {
	u := strings.TrimRight(base, "/") + "/api/agents/current-reply/" + url.PathEscape(paneID) + "?token=" + url.QueryEscape(apiToken)
	resp, err := hc.Get(u)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var d struct {
		Status        string  `json:"status"`
		Model         string  `json:"model"`
		ContextPct    float64 `json:"context_used_pct"`
		ContextWindow int     `json:"context_window_size"`
		InputTokens   int64   `json:"input_tokens"`
	}
	if json.Unmarshal(body, &d) != nil {
		return
	}
	if d.Status != "" {
		a.Status = d.Status
	}
	a.Model = d.Model
	a.ContextPct = d.ContextPct
	a.ContextWindow = d.ContextWindow
	a.InputTokens = d.InputTokens
}
