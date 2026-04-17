package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DefaultCredPath returns the default path for storing bot credentials.
func DefaultCredPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-bridge", "credentials.json")
}

// AmbientUserPath returns the file path for persisting the ambient user ID.
func AmbientUserPath(dir string) string {
	return filepath.Join(dir, "ambient-user.txt")
}

// ContextTokensPath returns the file path for persisted per-user context tokens.
func ContextTokensPath(dir string) string {
	return filepath.Join(dir, "context-tokens.json")
}

// SaveAmbientUser persists the ambient WeChat user ID to disk.
func SaveAmbientUser(userID, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(userID), 0600)
}

// LoadAmbientUser reads the persisted ambient user ID (empty string if not set).
func LoadAmbientUser(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SaveContextTokens persists per-user context tokens to disk.
func SaveContextTokens(tokens map[string]string, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadContextTokens reads persisted per-user context tokens from disk.
func LoadContextTokens(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	var tokens map[string]string
	if err := json.Unmarshal(data, &tokens); err != nil {
		return map[string]string{}
	}
	if tokens == nil {
		return map[string]string{}
	}
	return tokens
}
