package app

import (
	"fmt"
	"os"
	"path/filepath"
)

const Name = "claude-bridge"

// AssetRoot returns the directory that contains runtime assets such as
// scripts/ and hooks/ for both source checkouts and Homebrew installs.
func AssetRoot() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}

	exeDir := filepath.Dir(exePath)
	prefixDir := filepath.Dir(exeDir)

	candidates := []string{
		filepath.Join(prefixDir, "share", Name), // Homebrew pkgshare
		prefixDir,                               // local bin/ layout
	}

	for _, root := range candidates {
		if isDir(filepath.Join(root, "scripts")) && isDir(filepath.Join(root, "hooks")) {
			return root, nil
		}
	}

	return "", fmt.Errorf("runtime assets not found near executable %s", exePath)
}

func AssetPath(parts ...string) (string, error) {
	root, err := AssetRoot()
	if err != nil {
		return "", err
	}
	all := append([]string{root}, parts...)
	return filepath.Join(all...), nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
