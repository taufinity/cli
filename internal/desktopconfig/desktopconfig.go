// Package desktopconfig reads, updates, and writes Claude Desktop's MCP server
// config file (claude_desktop_config.json) atomically and without clobbering
// unrelated entries.
package desktopconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// RemoteServer matches the JSON shape Claude Desktop expects for HTTP MCP servers,
// per docs/mcp/remote-setup.md.
type RemoteServer struct {
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// StdioServer matches the JSON shape Claude Desktop expects for a stdio
// MCP server: a subprocess invocation. The CLI uses this to install itself
// as a bridge (`taufinity mcp stdio`), which Claude Desktop launches and
// which forwards JSON-RPC frames to Studio's /mcp endpoint. The bearer
// token is NOT embedded — the bridge subprocess reads credentials.json at
// startup.
type StdioServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// ErrUnsupportedOS indicates Claude Desktop is not available on the current OS.
var ErrUnsupportedOS = errors.New("Claude Desktop is not available on this OS; use --client print")

// DefaultClaudeDesktopPath returns the per-OS path to Claude Desktop's config.
func DefaultClaudeDesktopPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA not set")
		}
		return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
	default:
		return "", ErrUnsupportedOS
	}
}

// DefaultServersKey is the standard top-level key used by Claude Desktop,
// Claude Code, Cursor, and most other MCP-aware tools. VS Code uses "servers"
// instead — callers writing a VS Code config must pass that explicitly via
// the *InKey variants.
const DefaultServersKey = "mcpServers"

// UpsertServer reads the JSON file at path (creating it if absent), inserts or
// updates the named MCP server entry under "mcpServers", and writes the result
// atomically (temp file + rename). All other top-level keys and sibling server
// entries are preserved.
//
// server can be any JSON-marshalable value; in practice either RemoteServer
// (HTTP) or StdioServer (stdio bridge). The caller picks the shape; this
// function does not validate it.
func UpsertServer(path, name string, server any) error {
	return UpsertServerInKey(path, DefaultServersKey, name, server)
}

// RemoveServer removes the named entry from "mcpServers". No-op if the file or
// entry is absent.
func RemoveServer(path, name string) error {
	return RemoveServerInKey(path, DefaultServersKey, name)
}

// HasServer returns true if the named entry exists in "mcpServers".
func HasServer(path, name string) (bool, error) {
	return HasServerInKey(path, DefaultServersKey, name)
}

// UpsertServerInKey is the generalised form of UpsertServer that lets the
// caller pick the top-level JSON key. VS Code's MCP config uses "servers";
// every other supported client uses "mcpServers" (see DefaultServersKey).
func UpsertServerInKey(path, key, name string, server any) error {
	doc, err := readDoc(path)
	if err != nil {
		return err
	}
	servers, _ := doc[key].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[name] = server
	doc[key] = servers
	return writeDocAtomic(path, doc)
}

// RemoveServerInKey removes the named entry from the given top-level key.
// No-op if the file, key, or entry is absent.
func RemoveServerInKey(path, key, name string) error {
	doc, err := readDoc(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	servers, _ := doc[key].(map[string]any)
	if servers != nil {
		delete(servers, name)
		doc[key] = servers
	}
	return writeDocAtomic(path, doc)
}

// HasServerInKey returns true if the named entry exists under the given key.
func HasServerInKey(path, key, name string) (bool, error) {
	doc, err := readDoc(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	servers, _ := doc[key].(map[string]any)
	_, ok := servers[name]
	return ok, nil
}

func readDoc(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc, nil
}

func writeDocAtomic(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
