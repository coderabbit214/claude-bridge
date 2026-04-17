package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Info is a filesystem-derived snapshot of a currently discoverable session.
type Info struct {
	ID          string
	WorkDir     string
	Interactive bool
	Running     bool
	Source      string
	PID         int
}

// Inspect enumerates sessions that are currently visible through the runtime
// registry and ambient hook pipes, even when the bridge process is not running
// in the foreground.
func Inspect() ([]Info, error) {
	var infos []Info
	seen := make(map[string]bool)

	if entries, err := os.ReadDir(registryDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(registryDir, e.Name())
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var manifest registeredSessionManifest
			if err := json.Unmarshal(b, &manifest); err != nil || manifest.ID == "" {
				continue
			}
			info := Info{
				ID:          manifest.ID,
				WorkDir:     manifest.WorkDir,
				Interactive: manifest.InputPipe != "",
				Running:     manifest.PID <= 0 || isPidAlive(manifest.PID),
				Source:      "registered",
				PID:         manifest.PID,
			}
			infos = append(infos, info)
			seen[info.ID] = true
		}
	}

	if entries, err := os.ReadDir(ambientDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".pipe") {
				continue
			}
			sid := strings.TrimSuffix(e.Name(), ".pipe")
			if seen[sid] {
				continue
			}
			info := Info{
				ID:          sid,
				WorkDir:     getProcCWD(pidFromAmbientPipeName(e.Name())),
				Interactive: false,
				Running:     true,
				Source:      "ambient",
				PID:         pidFromAmbientPipeName(e.Name()),
			}
			if info.WorkDir == "" {
				info.WorkDir = "(local)"
			}
			infos = append(infos, info)
		}
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos, nil
}

func pidFromAmbientPipeName(name string) int {
	pidStr := strings.TrimSuffix(name, ".pipe")
	pid, _ := strconv.Atoi(pidStr)
	return pid
}
