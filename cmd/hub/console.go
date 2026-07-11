// Copyright 2026 CiCy AI
// SPDX-License-Identifier: Apache-2.0

// The operator console, embedded in the binary and served at /_console. It's a
// single self-contained HTML page: the operator opens it, pastes a hub token,
// and it loads the live agent directory from /_agents (same origin). The page is
// public; the data behind it (/_agents) is gated by the hub token.
package main

import (
	_ "embed"
	"net/http"
)

//go:embed console.html
var consoleHTML []byte

func consoleHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(consoleHTML)
}
