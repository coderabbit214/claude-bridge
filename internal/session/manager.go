package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coderabbit214/claude-bridge/internal/app"
)

type pendingInteraction struct {
	Kind      string
	ReplyPipe string
}

const replacedInteractionToken = "__claude_bridge_replaced__"

// Session represents one running Claude Code process.
type Session struct {
	ID      string
	WorkDir string
	InPipe  string
	proc    *exec.Cmd
	writer  io.WriteCloser
	cancel  context.CancelFunc
	alive   bool // true while the session goroutine is running
}

// Write sends a line of text to Claude Code's stdin.
func (s *Session) Write(text string) error {
	if s.writer == nil && s.InPipe == "" {
		return fmt.Errorf("session #%s does not support remote input (start via claude-bridge-local.sh to enable bidirectional interaction)", s.ID)
	}
	if s.writer == nil {
		writer, err := openPipeWriter(s.InPipe, 8*time.Second)
		if err != nil {
			return fmt.Errorf("connect input pipe: %w", err)
		}
		s.writer = writer
	}
	slog.Info("session input", "id", s.ID, "chars", len([]rune(text)))
	_, err := fmt.Fprintln(s.writer, text)
	return err
}

// IsAlive reports whether the process is still running.
func (s *Session) IsAlive() bool {
	if s.alive {
		return true
	}
	if s.writer != nil {
		return true
	}
	if s.InPipe != "" {
		return true
	}
	return s.proc != nil && s.proc.ProcessState == nil
}

// ── Manager ──────────────────────────────────────────────────────────────────

// OutputFunc is called when a hook pushes output for a session.
// Must be safe to call from multiple goroutines.
type OutputFunc func(sessionID, text string)

// Manager owns all active Claude Code sessions.
type Manager struct {
	mu             sync.Mutex
	sessions       map[string]*Session
	ambientPipes   map[string]bool   // pipePath → being watched
	manifestIDs    map[string]string // manifest/hook ID → short session ID
	targetID       string
	defaultSession string
	pending        map[string]pendingInteraction
	onOutput       OutputFunc
	nextID         int
}

// New creates a Manager.
func New(onOutput OutputFunc) *Manager {
	return &Manager{
		sessions:     make(map[string]*Session),
		ambientPipes: make(map[string]bool),
		manifestIDs:  make(map[string]string),
		pending:      make(map[string]pendingInteraction),
		onOutput:     onOutput,
	}
}

// resolveManifestID translates a hook/manifest session ID to the bridge's
// short session ID. Returns the input unchanged if no mapping exists.
func (m *Manager) resolveManifestID(manifestID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if shortID, ok := m.manifestIDs[manifestID]; ok {
		return shortID
	}
	return manifestID
}

// submitInteraction writes the user's choice to a waiting interaction pipe.
// Returns (true, reply) when the choice was accepted, (false, "") otherwise.
func (m *Manager) submitInteraction(sid, choice string) (bool, string) {
	m.mu.Lock()
	pending, ok := m.pending[sid]
	m.mu.Unlock()
	if !ok || pending.ReplyPipe == "" {
		return false, ""
	}

	writer, err := openPipeWriter(pending.ReplyPipe, 2*time.Second)
	if err != nil {
		slog.Warn("write interaction response failed", "sid", sid, "err", err)
		m.clearPendingInteraction(sid, pending.ReplyPipe)
		return true, "interaction expired, please retry"
	}
	defer writer.Close()
	if _, err := fmt.Fprintln(writer, choice); err != nil {
		slog.Warn("write interaction response failed", "sid", sid, "err", err)
		m.clearPendingInteraction(sid, pending.ReplyPipe)
		return true, "interaction expired, please retry"
	}

	if pending.Kind == "permission" {
		if choice == "1" {
			return true, "allowed"
		}
		return true, "denied"
	}
	return true, fmt.Sprintf("choice submitted: %s", choice)
}

// findSinglePending returns the sid of the only session currently waiting for
// a user reply, or "" if zero or more than one session is waiting.
func (m *Manager) findSinglePending() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := ""
	for sid := range m.pending {
		if found != "" && found != sid {
			return ""
		}
		found = sid
	}
	return found
}

// hasPending reports whether any interaction is pending for the given sid.
func (m *Manager) hasPending(sid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.pending[sid]
	return ok
}

func (m *Manager) setPendingInteraction(sid, kind, replyPipe string) {
	if sid == "" || replyPipe == "" {
		return
	}
	m.mu.Lock()
	prev, hadPrev := m.pending[sid]
	m.pending[sid] = pendingInteraction{Kind: kind, ReplyPipe: replyPipe}
	m.mu.Unlock()
	if hadPrev && prev.ReplyPipe != "" && prev.ReplyPipe != replyPipe {
		if writer, err := openPipeWriter(prev.ReplyPipe, 2*time.Second); err == nil {
			_, _ = fmt.Fprintln(writer, replacedInteractionToken)
			_ = writer.Close()
		}
	}
}

func (m *Manager) clearPendingInteraction(sid, replyPipe string) {
	if sid == "" {
		return
	}
	m.mu.Lock()
	current, ok := m.pending[sid]
	if !ok {
		m.mu.Unlock()
		return
	}
	if replyPipe != "" && current.ReplyPipe != replyPipe {
		m.mu.Unlock()
		return
	}
	delete(m.pending, sid)
	m.mu.Unlock()
}

// ParseCommand extracts a session ID and optional command from a WeChat message.
//
//	"#proj1 help me"  → ("proj1", "help me")
//	"#proj1"          → ("proj1", "")
func ParseCommand(text string) (sid, cmd string) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "#") {
		return "", ""
	}
	rest := t[1:]
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		return rest[:idx], strings.TrimSpace(rest[idx+1:])
	}
	return rest, ""
}

// Dispatch routes a WeChat message to the right Claude Code process.
// Returns an immediate reply string (e.g. session list) or "".
//
// Commands (prefix #):
//
//	#l / #list         – list active sessions
//	#n / #new <dir>    – create a new session at <dir>
//	#r / #reset        – clear the sticky default session
//	#<sid>             – set <sid> as the sticky default session
//	#<sid> <text>      – send text to a named session
//	plain text         – send to the sticky default session (if set)
func (m *Manager) Dispatch(text string) (string, error) {
	t := strings.TrimSpace(text)

	// Mobile keyboards often insert a newline instead of a space after the
	// command prefix (e.g. "#01\nhello" or "#n\n~/path").  Normalize all
	// internal whitespace to a single space for # commands so the rest of
	// the parsing logic doesn't need to handle newlines.
	if strings.HasPrefix(t, "#") {
		t = strings.Join(strings.Fields(t), " ")
	}

	if t == "#l" || t == "#list" {
		return m.ListSessions(), nil
	}

	if t == "#r" || t == "#reset" {
		m.mu.Lock()
		m.defaultSession = ""
		m.mu.Unlock()
		return "Default session cleared; prefix subsequent messages with #<sid>", nil
	}

	if after, ok := strings.CutPrefix(t, "#n "); ok {
		workdir := strings.TrimSpace(after)
		if workdir == "" {
			workdir = "."
		}
		sid, reply, err := m.newSession(workdir)
		_ = sid
		if err != nil {
			return "", err
		}
		return reply, nil
	}
	if after, ok := strings.CutPrefix(t, "#new "); ok {
		workdir := strings.TrimSpace(after)
		if workdir == "" {
			workdir = "."
		}
		sid, reply, err := m.newSession(workdir)
		_ = sid
		if err != nil {
			return "", err
		}
		return reply, nil
	}

	// Plain text → route to sticky default session.
	if !strings.HasPrefix(t, "#") {
		m.mu.Lock()
		defaultSid := m.defaultSession
		m.mu.Unlock()

		// Check default session's pending interaction first.
		if defaultSid != "" {
			if handled, reply := m.submitInteraction(defaultSid, t); handled {
				return reply, nil
			}
		}

		// Fallback: if exactly one session (including local) is waiting,
		// route the reply there automatically.
		if pendingSid := m.findSinglePending(); pendingSid != "" && pendingSid != defaultSid {
			if handled, reply := m.submitInteraction(pendingSid, t); handled {
				return reply, nil
			}
		}

		if defaultSid == "" {
			return "", fmt.Errorf("no default session set; create one with #n <dir> or send #<sid> to set default")
		}
		sess, err := m.getOrCreate(defaultSid, ".")
		if err != nil {
			return "", err
		}
		return "", sess.Write(t)
	}

	sid, cmd := ParseCommand(t)

	// "#<sid>" alone (no command) → set as sticky default.
	if sid != "" && cmd == "" {
		m.mu.Lock()
		_, exists := m.sessions[sid]
		m.mu.Unlock()
		// Also allow local/ambient sessions that have a pending interaction.
		if !exists && !m.hasPending(sid) {
			return "", fmt.Errorf("session #%s does not exist; create one with #n <dir>", sid)
		}
		m.mu.Lock()
		m.defaultSession = sid
		m.mu.Unlock()
		return fmt.Sprintf("#%s set as default session; just send messages directly\n(send #r to clear default)", sid), nil
	}

	if sid == "" || cmd == "" {
		return "", fmt.Errorf("invalid command; use #l, #n <dir>, #<sid>, or #<sid> <text>")
	}

	if handled, reply := m.submitInteraction(sid, cmd); handled {
		return reply, nil
	}

	sess, err := m.getOrCreate(sid, ".")
	if err != nil {
		return "", err
	}
	return "", sess.Write(cmd)
}

// ListSessions returns a human-readable overview of interactive sessions.
func (m *Manager) ListSessions() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	defaultSid := m.defaultSession
	if len(m.sessions) == 0 {
		return "No active sessions. Send #n ~/proj to create one"
	}
	var sb strings.Builder
	sb.WriteString("📋 Active sessions:\n")
	for id, s := range m.sessions {
		status := "running"
		if !s.IsAlive() {
			status = "stopped"
		}
		tag := ""
		if id == defaultSid {
			tag = " [default]"
		}
		sb.WriteString(fmt.Sprintf("  #%s  %s  %s%s\n", id, s.WorkDir, status, tag))
	}
	if defaultSid == "" {
		sb.WriteString("\nsend #<sid> to set default session")
	}
	return sb.String()
}

// getOrCreate returns an existing alive session or starts a new one.
func (m *Manager) getOrCreate(sid, workDir string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sid]; ok && s.IsAlive() {
		return s, nil
	}
	if workDir == "." {
		return nil, fmt.Errorf("session #%s does not exist; create one with #n <dir>", sid)
	}
	return m.start(sid, workDir)
}

func (m *Manager) newSession(workDir string) (sid, reply string, err error) {
	// Expand ~ so Stat and display path are both absolute.
	if workDir == "~" || strings.HasPrefix(workDir, "~/") {
		if home, herr := os.UserHomeDir(); herr == nil {
			workDir = filepath.Join(home, workDir[1:])
		}
	}
	if _, serr := os.Stat(workDir); os.IsNotExist(serr) {
		return "", "", fmt.Errorf("directory does not exist: %s", workDir)
	}
	m.mu.Lock()
	m.nextID++
	sid = fmt.Sprintf("%02d", m.nextID)
	m.mu.Unlock()
	if _, err = m.createSession(sid, workDir); err != nil {
		return "", "", fmt.Errorf("create session: %w", err)
	}
	m.mu.Lock()
	m.defaultSession = sid
	m.mu.Unlock()
	reply = fmt.Sprintf("Session #%s created\nDir: %s\nSet as default; just send messages directly\n(send #r to clear default)", sid, workDir)
	return sid, reply, nil
}

func (m *Manager) createSession(sid, workDir string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sid]; ok && s.IsAlive() {
		return s, nil
	}
	return m.start(sid, workDir)
}

// start launches a claude process and a named-pipe reader.
// Caller must hold m.mu.
func (m *Manager) start(sid, workDir string) (*Session, error) {
	ctx, cancel := context.WithCancel(context.Background())

	pipePath := fmt.Sprintf("/tmp/claude-hook-%s.pipe", sid)
	inputPipePath := fmt.Sprintf("/tmp/claude-input-%s.pipe", sid)
	os.Remove(pipePath)
	os.Remove(inputPipePath)
	if err := syscall.Mkfifo(pipePath, 0600); err != nil {
		cancel()
		return nil, fmt.Errorf("mkfifo: %w", err)
	}
	if err := syscall.Mkfifo(inputPipePath, 0600); err != nil {
		cancel()
		_ = os.Remove(pipePath)
		return nil, fmt.Errorf("mkfifo input: %w", err)
	}

	if err := launchClaudeInTerminal(workDir, sid, pipePath, inputPipePath); err != nil {
		cancel()
		_ = os.Remove(pipePath)
		_ = os.Remove(inputPipePath)
		return nil, fmt.Errorf("open terminal: %w", err)
	}

	s := &Session{
		ID:      sid,
		WorkDir: workDir,
		InPipe:  inputPipePath,
		cancel:  cancel,
	}
	m.sessions[sid] = s

	go func() {
		m.readPipe(ctx, pipePath, sid)
		// readPipe returned → output pipe was removed (Claude exited).
		m.mu.Lock()
		if s, ok := m.sessions[sid]; ok {
			if s.cancel != nil {
				s.cancel()
			}
			if s.writer != nil {
				_ = s.writer.Close()
			}
		}
		delete(m.sessions, sid)
		delete(m.pending, sid)
		m.mu.Unlock()
		slog.Info("session cleaned up", "id", sid)
	}()

	slog.Info("session started", "id", sid, "cwd", workDir)
	return s, nil
}

func launchClaudeInTerminal(workDir, sid, outputPipePath, inputPipePath string) error {
	launchDir := "/tmp/claude-bridge-launch"
	if err := os.MkdirAll(launchDir, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(registryDir, 0700); err != nil {
		return err
	}

	muxPath, err := app.AssetPath("scripts", "claude_bridge_mux.py")
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(registryDir, sid+".json")
	scriptPath := filepath.Join(launchDir, sid+".command")
	scriptBody := fmt.Sprintf(`#!/bin/zsh
set -euo pipefail
cleanup() {
  rm -f %s %s %s
}
trap cleanup EXIT INT TERM
cd %s
export CLAUDE_SESSION_ID=%s
export CLAUDE_HOOK_PIPE=%s
cat > %s <<EOF
{"id":"%s","pid":$$,"work_dir":%s,"input_pipe":%s,"output_pipe":%s}
EOF
printf '%%s\n' %s
python3 %s %s -- claude
`, shellQuote(inputPipePath), shellQuote(outputPipePath), shellQuote(manifestPath),
		shellQuote(workDir),
		shellQuote(sid),
		shellQuote(outputPipePath),
		shellQuote(manifestPath),
		sid,
		strconv.Quote(workDir),
		strconv.Quote(inputPipePath),
		strconv.Quote(outputPipePath),
		shellQuote("[claude-bridge] session #"+sid+" ready"),
		shellQuote(muxPath),
		shellQuote(inputPipePath),
	)
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0700); err != nil {
		return err
	}

	cmd := exec.Command("open", "-a", "Terminal", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func openPipeWriter(path string, timeout time.Duration) (io.WriteCloser, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.ENXIO) && !errors.Is(err, syscall.ENOENT) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// readPipe reads output blocks from the hook script via a named pipe.
// The hook writes lines and terminates each block with "---END---\n".
func (m *Manager) readPipe(ctx context.Context, pipePath, sid string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		f, err := os.Open(pipePath) // blocks until hook opens write end
		if err != nil {
			if os.IsNotExist(err) {
				// Pipe file was removed — the session has ended; stop looping.
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
				continue
			}
		}

		var buf strings.Builder
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 128*1024), 128*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "---END---" {
				if text := strings.TrimSpace(buf.String()); text != "" {
					m.onOutput(sid, text)
				}
				buf.Reset()
			} else {
				buf.WriteString(line)
				buf.WriteByte('\n')
			}
		}
		if text := strings.TrimSpace(buf.String()); text != "" {
			m.onOutput(sid, text)
		}
		f.Close()
	}
}
