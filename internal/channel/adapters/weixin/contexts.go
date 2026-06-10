package weixin

// The iLink protocol requires a context_token on every outbound message, and
// tokens only arrive on inbound messages. The receive loop used to keep them
// purely in memory, which made proactive pushes (channel.SendOnce from octo
// serve, a different process) impossible. This file is the write-through disk
// store that bridges the two processes: the receive loop persists the latest
// token per user here, and a standalone send reads it back.
//
// How long WeChat honours an old context_token is not documented; a send with
// a stale token fails with an API error, which callers surface or log. Tokens
// stay fresh as long as the user keeps talking to the bot while the weixin
// channel is connected.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// contextStorePath derives the token-store path from the credentials path so
// both live in the same directory (default ~/.octo/).
func contextStorePath(credPath string) string {
	return filepath.Join(filepath.Dir(credPath), "weixin-contexts.json")
}

// loadContextTokens reads the store. A missing or unreadable file is an empty
// map — the caller's lookup then fails with its own, clearer error.
func loadContextTokens(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if json.Unmarshal(data, &m) != nil || m == nil {
		return map[string]string{}
	}
	return m
}

// saveContextToken records the latest context_token for a user. Writes go
// through a temp file + rename so a concurrent reader in another process
// never sees a torn file. Errors are swallowed: the in-memory token still
// serves the running process, the store is best-effort.
func saveContextToken(path, userID, token string) {
	if path == "" || userID == "" || token == "" {
		return
	}
	m := loadContextTokens(path)
	if m[userID] == token {
		return
	}
	m[userID] = token
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
