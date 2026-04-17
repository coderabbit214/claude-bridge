package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const ambientDir = "/tmp/claude-bridge-hooks"
const registryDir = "/tmp/claude-bridge-sessions"
const controlPipePath = "/tmp/claude-bridge-control.pipe"

type registeredSessionManifest struct {
	ID         string `json:"id"`
	PID        int    `json:"pid"`
	WorkDir    string `json:"work_dir"`
	InputPipe  string `json:"input_pipe"`
	OutputPipe string `json:"output_pipe"`
}

// SetTarget sets the single message target that receives output from the bridge.
func (m *Manager) SetTarget(targetID string) {
	m.mu.Lock()
	m.targetID = targetID
	m.mu.Unlock()
}

// GetTarget returns the current message target (empty if unset).
func (m *Manager) GetTarget() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.targetID
}

// StartAmbientWatcher initialises local-session support:
//  1. Removes stale manifests left by dead processes.
//  2. Runs a one-time startup scan to recover sessions that were alive when
//     the bridge last stopped (bridge-restart recovery).
//  3. Starts readControlPipe — the primary, event-driven registration path.
//     SessionStart hooks write a JSON command to the control pipe; the bridge
//     reacts immediately without polling.
func (m *Manager) StartAmbientWatcher(ctx context.Context) {
	os.MkdirAll(ambientDir, 0700)
	os.MkdirAll(registryDir, 0700)
	m.cleanupStaleManifests()
	m.startupScan(ctx)
	go m.readControlPipe(ctx)
}

// startupScan registers sessions that were already running when the bridge
// started (or restarted). Called once; not a periodic loop.
func (m *Manager) startupScan(ctx context.Context) {
	m.mu.Lock()
	targetID := m.targetID
	m.mu.Unlock()
	if targetID == "" {
		return
	}

	// Manifest-based sessions (full input+output support).
	if entries, err := os.ReadDir(registryDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(registryDir, e.Name()))
			if err != nil {
				continue
			}
			var manifest registeredSessionManifest
			if err := json.Unmarshal(b, &manifest); err != nil {
				continue
			}
			m.registerManifestSession(ctx, manifest)
		}
	}

	// Legacy ambient-pipe sessions (output-only; no manifest).
	if entries, err := os.ReadDir(ambientDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".pipe") {
				continue
			}
			pipePath := filepath.Join(ambientDir, e.Name())
			manifestSID := "local-" + strings.TrimSuffix(e.Name(), ".pipe")

			m.mu.Lock()
			already := m.ambientPipes[pipePath]
			_, mapped := m.manifestIDs[manifestSID]
			if !already && !mapped {
				m.ambientPipes[pipePath] = true
			}
			m.mu.Unlock()

			if already || mapped {
				continue
			}
			// Skip if there is already a manifest for this sid (already handled above).
			if _, err := os.Stat(filepath.Join(registryDir, manifestSID+".json")); err == nil {
				continue
			}
			slog.Info("ambient session detected", "pipe", pipePath)
			go m.readAmbientPipe(ctx, pipePath, manifestSID)
		}
	}
}

// readControlPipe is the primary, event-driven session-registration path.
//
// The bridge creates a control FIFO and holds it open for reading (O_RDWR so
// the read side never gets EOF when hook processes come and go). SessionStart
// hooks write a JSON line; the bridge parses it and registers the session
// immediately — no polling needed.
//
// Message format (one JSON object per line):
//
//	{"action":"register","id":"...","pid":N,"work_dir":"...","input_pipe":"...","output_pipe":"..."}
func (m *Manager) readControlPipe(ctx context.Context) {
	// Remove any pipe left from a previous run, then recreate.
	os.Remove(controlPipePath)
	if err := syscall.Mkfifo(controlPipePath, 0600); err != nil {
		slog.Error("control pipe mkfifo failed", "err", err)
		return
	}
	defer os.Remove(controlPipePath)

	// O_RDWR keeps the write end open inside this process, so Scan() never
	// returns io.EOF when an external hook writer closes its end.
	f, err := os.OpenFile(controlPipePath, os.O_RDWR, os.ModeNamedPipe)
	if err != nil {
		slog.Error("control pipe open failed", "err", err)
		return
	}
	defer f.Close()

	slog.Info("control pipe ready", "path", controlPipePath)

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case line := <-lines:
			m.handleControlCommand(ctx, line)
		}
	}
}

// handleControlCommand parses one line from the control pipe and acts on it.
func (m *Manager) handleControlCommand(ctx context.Context, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	var msg struct {
		Action    string `json:"action"`
		Kind      string `json:"kind"`
		ReplyPipe string `json:"reply_pipe"`
		registeredSessionManifest
	}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		slog.Warn("control pipe: bad JSON", "line", line, "err", err)
		return
	}

	switch msg.Action {
	case "", "register":
		m.mu.Lock()
		targetID := m.targetID
		m.mu.Unlock()
		if targetID == "" {
			slog.Warn("control pipe: no target set, dropping register", "sid", msg.ID)
			return
		}
		m.registerManifestSession(ctx, msg.registeredSessionManifest)
	case "unregister":
		m.unregisterSession(msg.ID)
	case "interaction_start":
		m.setPendingInteraction(m.resolveManifestID(msg.ID), msg.Kind, msg.ReplyPipe)
	case "interaction_end":
		m.clearPendingInteraction(m.resolveManifestID(msg.ID), msg.ReplyPipe)
	default:
		slog.Warn("control pipe: unknown action", "action", msg.Action)
	}
}

// registerManifestSession is the single place that turns a manifest into a
// live Session. Called from both startupScan and handleControlCommand.
// It assigns a new short session ID regardless of the manifest's original ID.
func (m *Manager) registerManifestSession(ctx context.Context, manifest registeredSessionManifest) {
	if manifest.ID == "" || manifest.InputPipe == "" || manifest.OutputPipe == "" {
		return
	}

	m.mu.Lock()
	_, alreadyMapped := m.manifestIDs[manifest.ID]
	already := m.ambientPipes[manifest.OutputPipe]
	var shortID string
	if !alreadyMapped && !already {
		m.ambientPipes[manifest.OutputPipe] = true
		m.nextID++
		shortID = fmt.Sprintf("%02d", m.nextID)
		m.manifestIDs[manifest.ID] = shortID
	}
	m.mu.Unlock()

	if alreadyMapped || already {
		return
	}

	writer, err := openPipeWriter(manifest.InputPipe, 2*time.Second)
	if err != nil {
		slog.Warn("registerManifestSession: cannot open input pipe", "sid", shortID, "err", err)
		m.mu.Lock()
		delete(m.ambientPipes, manifest.OutputPipe)
		delete(m.manifestIDs, manifest.ID)
		m.mu.Unlock()
		return
	}

	s := &Session{
		ID:      shortID,
		WorkDir: manifest.WorkDir,
		InPipe:  manifest.InputPipe,
		writer:  writer,
		cancel:  nil,
	}
	m.mu.Lock()
	m.sessions[shortID] = s
	m.mu.Unlock()

	slog.Info("session registered", "sid", shortID, "cwd", manifest.WorkDir)
	go m.readRegisteredSession(ctx, manifest, shortID)
}

func (m *Manager) unregisterSession(manifestID string) {
	if manifestID == "" {
		return
	}
	m.mu.Lock()
	shortID, ok := m.manifestIDs[manifestID]
	if !ok {
		m.mu.Unlock()
		return
	}
	s, ok := m.sessions[shortID]
	m.mu.Unlock()
	if !ok {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	if outPipe := sessionOutputPipe(manifestID); outPipe != "" {
		unblockPipe(outPipe)
	}
	slog.Info("session unregistered", "sid", shortID)
}

// readAmbientPipe handles legacy output-only sessions (no manifest, no input pipe).
func (m *Manager) readAmbientPipe(ctx context.Context, pipePath, manifestSID string) {
	pidStr := strings.TrimSuffix(filepath.Base(pipePath), ".pipe")
	pid, _ := strconv.Atoi(pidStr)

	workDir := getProcCWD(pid)
	if workDir == "" {
		workDir = "(local)"
	}

	m.mu.Lock()
	m.nextID++
	shortID := fmt.Sprintf("%02d", m.nextID)
	m.manifestIDs[manifestSID] = shortID
	m.sessions[shortID] = &Session{
		ID:      shortID,
		WorkDir: workDir,
		alive:   true,
	}
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		if s, ok := m.sessions[shortID]; ok {
			s.alive = false
		}
		delete(m.sessions, shortID)
		delete(m.ambientPipes, pipePath)
		delete(m.manifestIDs, manifestSID)
		m.mu.Unlock()
		os.Remove(pipePath)
		slog.Info("ambient session cleaned up", "sid", shortID)
	}()

	m.readPipe(ctx, pipePath, shortID)
}

func (m *Manager) readRegisteredSession(ctx context.Context, manifest registeredSessionManifest, shortID string) {
	ctx2, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if s, ok := m.sessions[shortID]; ok {
		s.cancel = cancel
	}
	m.mu.Unlock()
	defer cancel()
	defer func() {
		m.mu.Lock()
		delete(m.ambientPipes, manifest.OutputPipe)
		if s, ok := m.sessions[shortID]; ok {
			if s.writer != nil {
				_ = s.writer.Close()
			}
			delete(m.sessions, shortID)
		}
		delete(m.pending, shortID)
		delete(m.manifestIDs, manifest.ID)
		m.mu.Unlock()
		slog.Info("registered session cleaned up", "sid", shortID)
	}()

	m.readPipe(ctx2, manifest.OutputPipe, shortID)
}

func sessionOutputPipe(sid string) string {
	if sid == "" {
		return ""
	}
	manifestPath := filepath.Join(registryDir, sid+".json")
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Sprintf("/tmp/claude-hook-%s.pipe", sid)
	}
	var manifest registeredSessionManifest
	if err := json.Unmarshal(b, &manifest); err != nil || manifest.OutputPipe == "" {
		return fmt.Sprintf("/tmp/claude-hook-%s.pipe", sid)
	}
	return manifest.OutputPipe
}

// cleanupStaleManifests removes manifests (and their pipes) for processes that
// are no longer alive. Called once at startup.
func (m *Manager) cleanupStaleManifests() {
	entries, err := os.ReadDir(registryDir)
	if err != nil {
		return
	}
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
		if err := json.Unmarshal(b, &manifest); err != nil {
			continue
		}
		if manifest.PID > 0 && !isPidAlive(manifest.PID) {
			os.Remove(path)
			os.Remove(manifest.InputPipe)
			os.Remove(manifest.OutputPipe)
			slog.Info("stale manifest removed", "sid", manifest.ID, "pid", manifest.PID)
		}
	}
}

// unblockPipe briefly opens the write end of a named pipe so any goroutine
// blocked in os.Open (read side) will unblock and see ctx.Done().
func unblockPipe(pipePath string) {
	f, err := os.OpenFile(pipePath, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		f.Close()
	}
}

// isPidAlive reports whether the process is still running.
func isPidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// getProcCWD returns the working directory of the given process on macOS.
func getProcCWD(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-a", "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}
