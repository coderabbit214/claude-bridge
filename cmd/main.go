package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"

	wechatbot "github.com/corespeed-io/wechatbot/golang"

	"github.com/coderabbit214/claude-bridge/internal/app"
	"github.com/coderabbit214/claude-bridge/internal/platform"
	"github.com/coderabbit214/claude-bridge/internal/session"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return runStart(nil)
	}

	switch args[0] {
	case "login":
		return runLogin(args[1:])
	case "start":
		return runStart(args[1:])
	case "serve":
		return runBridge(args[1:])
	case "list":
		return runList(args[1:])
	case "logs":
		return runLogs(args[1:])
	case "install-hooks":
		return runInstallHooks(args[1:])
	case "status":
		return runStatus(args[1:])
	case "stop":
		return runStop(args[1:])
	case "clear":
		return runClear(args[1:])
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	credsPath := fs.String("creds", platform.DefaultCredPath(), "Path to credentials JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if _, err := os.Stat(*credsPath); err == nil {
		fmt.Println("✅ Already logged in, no need to scan QR code again.")
		fmt.Println("   To re-login, delete:", *credsPath)
		return nil
	}

	bot := wechatbot.New(wechatbot.Options{
		CredPath: *credsPath,
		LogLevel: "warn",
		OnQRURL: func(url string) {
			printTerminalQR(url)
			fmt.Printf("\n🔗 QR code URL: %s\n", url)
			fmt.Println("Please scan with WeChat, then tap 'Confirm Login' on your phone...")
		},
		OnScanned: func() {
			fmt.Println("QR code scanned, waiting for confirmation...")
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	creds, err := bot.Login(ctx, true)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	fmt.Printf("\n✅ WeChat connected! account_id=%s\n", creds.AccountID)
	fmt.Println("   Credentials saved to:", *credsPath)
	fmt.Println("\nNow run the main process:")
	fmt.Println("   ./bin/claude-bridge")
	return nil
}

func runBridge(args []string) error {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	credsPath := fs.String("creds", platform.DefaultCredPath(), "Path to credentials JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if _, err := os.Stat(*credsPath); err != nil {
		return fmt.Errorf("credentials not found — run './bin/claude-bridge login' first")
	}

	stateDir := filepath.Dir(*credsPath)
	pidPath := bridgePIDPath(stateDir)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0600); err != nil {
		return err
	}
	defer os.Remove(pidPath)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var adapter platform.Adapter = platform.NewILinkAdapter(*credsPath)

	sendToTarget := func(targetID, text string) {
		slog.Info("sending to user", "user", targetID, "chars", len([]rune(text)))
		if err := adapter.SendText(ctx, targetID, text); err != nil {
			slog.Error("send failed", "user", targetID, "err", err)
		}
	}

	var mgr *session.Manager
	mgr = session.New(func(sid, text string) {
		targetID := mgr.GetTarget()
		if targetID == "" {
			slog.Warn("dropping session output: no target set", "sid", sid)
			return
		}
		slog.Info("session output", "sid", sid, "user", targetID, "chars", len([]rune(text)))
		sender := "System"
		body := text
		if after, ok := strings.CutPrefix(text, "SENDER:"); ok {
			if idx := strings.Index(after, "\n"); idx >= 0 {
				sender = after[:idx]
				body = strings.TrimSpace(after[idx+1:])
			}
		}
		prefix := fmt.Sprintf("[%s] %s\n", sid, sender)
		sendToTarget(targetID, prefix+body)
	})

	ambientUserPath := platform.AmbientUserPath(stateDir)
	if targetID := platform.LoadAmbientUser(ambientUserPath); targetID != "" {
		mgr.SetTarget(targetID)
		slog.Info("target restored", "user", targetID)
	}
	mgr.StartAmbientWatcher(ctx)

	handleMsg := func(msg platform.Message) {
		targetID := msg.UserID
		if mgr.GetTarget() != targetID {
			mgr.SetTarget(targetID)
			if err := platform.SaveAmbientUser(targetID, ambientUserPath); err != nil {
				slog.Warn("save target failed", "err", err)
			}
		}

		text := msg.Text
		if text == "" {
			return
		}
		slog.Info("rx", "from", targetID, "text", truncateStr(text, 80))

		go func() {
			_ = adapter.SetTyping(ctx, targetID, true)
		}()

		reply, err := mgr.Dispatch(text)
		if err != nil {
			slog.Warn("dispatch failed", "user", targetID, "text", truncateStr(text, 80), "err", err)
			sendToTarget(targetID, "error: "+err.Error())
		} else if reply != "" {
			sendToTarget(targetID, reply)
		}

		go func() {
			time.Sleep(500 * time.Millisecond)
			_ = adapter.SetTyping(ctx, targetID, false)
		}()
	}

	go func() {
		if err := adapter.Run(ctx); err != nil && ctx.Err() == nil {
			if strings.Contains(err.Error(), "not logged in") {
				slog.Error("credentials format changed, please re-login",
					"hint", "rm "+*credsPath+" && ./bin/claude-bridge login")
			} else {
				slog.Error("adapter stopped", "err", err)
			}
			stop()
		}
	}()

	if restoredTarget := mgr.GetTarget(); restoredTarget != "" {
		go func(targetID string) {
			select {
			case <-ctx.Done():
				return
			case <-adapter.Ready():
			case <-time.After(5 * time.Second):
				return
			}
			sendToTarget(targetID, startupSummary(mgr))
		}(restoredTarget)
	}

	slog.Info("bridge running — send a WeChat DM to start")

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			_ = adapter.Close()
			return nil
		case msg := <-adapter.Events():
			go handleMsg(msg)
		}
	}
}

func startupSummary(mgr *session.Manager) string {
	sessionList := mgr.ListSessions()
	return strings.TrimSpace(fmt.Sprintf(`claude-bridge started

Available commands:
#l            list all sessions
#n .          create session in current directory
#n ~/path     create session in specified directory
#r            clear default session

Session shortcuts:
#<sid>        set as default session
#<sid> hello  send message to session

Current sessions:
%s`, sessionList))
}

func runStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	credsPath := fs.String("creds", platform.DefaultCredPath(), "Path to credentials JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	stateDir := filepath.Dir(*credsPath)
	pidPath := bridgePIDPath(stateDir)
	if pid, ok := readLivePID(pidPath); ok {
		fmt.Printf("bridge is already running in background, pid=%d\n", pid)
		return nil
	}

	logFile, err := openBridgeLogFile(stateDir)
	if err != nil {
		return err
	}
	defer logFile.Close()

	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	cmd := exec.Command(exePath, "serve", "--creds", *credsPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0600); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	_ = cmd.Process.Release()

	fmt.Printf("bridge started, pid=%d\n", cmd.Process.Pid)
	fmt.Printf("log file: %s\n", bridgeLogPath(stateDir))
	return nil
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}

	infos, err := session.Inspect()
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	fmt.Println("Active sessions:")
	for _, info := range infos {
		status := "stopped"
		if info.Running {
			status = "running"
		}
		mode := "output-only"
		if info.Interactive {
			mode = "interactive"
		}
		pidPart := ""
		if info.PID > 0 {
			pidPart = fmt.Sprintf(" pid=%d", info.PID)
		}
		fmt.Printf("#%s  %s  %s  source=%s%s\n", info.ID, info.WorkDir, status+"/"+mode, info.Source, pidPart)
	}
	return nil
}

func runLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	credsPath := fs.String("creds", platform.DefaultCredPath(), "Path to credentials JSON")
	lines := fs.Int("n", 200, "Number of trailing log lines")
	follow := fs.Bool("f", false, "Follow appended log output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logPath := bridgeLogPath(filepath.Dir(*credsPath))
	if _, err := os.Stat(logPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("log file not found: %s", logPath)
		}
		return err
	}

	if err := printLastLines(logPath, *lines); err != nil {
		return err
	}
	if !*follow {
		return nil
	}
	return followFile(logPath)
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	credsPath := fs.String("creds", platform.DefaultCredPath(), "Path to credentials JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	stateDir := filepath.Dir(*credsPath)
	pidPath := bridgePIDPath(stateDir)
	if pid, ok := readLivePID(pidPath); ok {
		fmt.Printf("bridge is running, pid=%d\n", pid)
		return nil
	}
	fmt.Println("bridge is not running")
	return nil
}

func runInstallHooks(args []string) error {
	fs := flag.NewFlagSet("install-hooks", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}

	hookSrc, err := app.AssetPath("hooks", "push_output.py")
	if err != nil {
		return err
	}
	settingsSrc, err := app.AssetPath("hooks", "settings.json")
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	destinations := []string{
		filepath.Join(home, ".claude", "hooks", "push_output.py"),
	}

	for _, dst := range destinations {
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		if err := copyFile(hookSrc, dst, 0755); err != nil {
			return err
		}
		fmt.Println("hook installed:", dst)
	}

	fmt.Println("")
	fmt.Println("Merge the following into ~/.claude/settings.json:")
	fmt.Println("")
	b, err := os.ReadFile(settingsSrc)
	if err != nil {
		return err
	}
	fmt.Print(string(b))
	if len(b) == 0 || b[len(b)-1] != '\n' {
		fmt.Println()
	}
	return nil
}

func runStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	credsPath := fs.String("creds", platform.DefaultCredPath(), "Path to credentials JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	stateDir := filepath.Dir(*credsPath)
	pidPath := bridgePIDPath(stateDir)
	pid, ok := readLivePID(pidPath)
	if !ok {
		_ = os.Remove(pidPath)
		fmt.Println("bridge is not running")
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	_ = os.Remove(pidPath)
	fmt.Printf("bridge stopped, pid=%d\n", pid)
	return nil
}

func runClear(args []string) error {
	fs := flag.NewFlagSet("clear", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	credsPath := fs.String("creds", platform.DefaultCredPath(), "Path to credentials JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	stateDir := filepath.Dir(*credsPath)

	// Stop the bridge first if it is running.
	pidPath := bridgePIDPath(stateDir)
	if pid, ok := readLivePID(pidPath); ok {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		fmt.Printf("bridge stopped, pid=%d\n", pid)
	}

	removed := 0

	// /tmp directories created by the bridge.
	tmpDirs := []string{
		"/tmp/claude-bridge-elicit",
		"/tmp/claude-bridge-perm",
		"/tmp/claude-bridge-launch",
		"/tmp/claude-bridge-hooks",
		"/tmp/claude-bridge-sessions",
	}
	for _, dir := range tmpDirs {
		if err := os.RemoveAll(dir); err == nil {
			fmt.Println("removed:", dir)
			removed++
		}
	}

	// Control pipe and dynamic session pipes.
	pipePaths, _ := filepath.Glob("/tmp/claude-bridge-control.pipe")
	hookPipes, _ := filepath.Glob("/tmp/claude-hook-*.pipe")
	inputPipes, _ := filepath.Glob("/tmp/claude-input-*.pipe")
	for _, p := range append(append(pipePaths, hookPipes...), inputPipes...) {
		if err := os.Remove(p); err == nil {
			fmt.Println("removed:", p)
			removed++
		}
	}

	// State files in stateDir (keep credentials.json and ambient-user.txt).
	stateFiles := []string{
		"bridge.log",
		"bridge.pid",
		"cursor.txt",
		//"context-tokens.json",
	}
	for _, name := range stateFiles {
		p := filepath.Join(stateDir, name)
		if err := os.Remove(p); err == nil {
			fmt.Println("removed:", p)
			removed++
		}
	}

	if removed == 0 {
		fmt.Println("No cache files to clean up.")
	} else {
		fmt.Printf("Cleanup complete, %d item(s) removed.\n", removed)
	}
	return nil
}

const (
	logMaxBytes  = 10 * 1024 * 1024 // 10 MB per file
	logKeepFiles = 5                // bridge.log.1 … bridge.log.5
)

func openBridgeLogFile(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	rotateBridgeLog(stateDir)
	return os.OpenFile(bridgeLogPath(stateDir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
}

func rotateBridgeLog(stateDir string) {
	current := bridgeLogPath(stateDir)
	info, err := os.Stat(current)
	if err != nil || info.Size() < logMaxBytes {
		return
	}
	for i := logKeepFiles - 1; i >= 1; i-- {
		os.Rename(
			fmt.Sprintf("%s.%d", current, i),
			fmt.Sprintf("%s.%d", current, i+1),
		)
	}
	os.Rename(current, current+".1")
}

func bridgeLogPath(stateDir string) string {
	return filepath.Join(stateDir, "bridge.log")
}

func bridgePIDPath(stateDir string) string {
	return filepath.Join(stateDir, "bridge.pid")
}

func printLastLines(path string, n int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		fmt.Println(line)
	}
	return nil
}

func followFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			fmt.Print(line)
			continue
		}
		if errors.Is(err, io.EOF) {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		return err
	}
}

func readLivePID(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, false
	}
	return pid, true
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: claude-bridge [command] [options]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  start   Start bridge in background (default)")
	fmt.Fprintln(w, "  status  Show bridge running status")
	fmt.Fprintln(w, "  stop    Stop the background bridge")
	fmt.Fprintln(w, "  login   Scan QR to log in to WeChat and save credentials")
	fmt.Fprintln(w, "  install-hooks  Install Claude hooks")
	fmt.Fprintln(w, "  list    List currently discoverable sessions")
	fmt.Fprintln(w, "  logs    View bridge logs")
	fmt.Fprintln(w, "  clear   Remove all cache files (keeps credentials and user config)")
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func printTerminalQR(url string) {
	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		fmt.Println("(cannot render QR code, use the URL above)")
		return
	}
	q.DisableBorder = false
	bm := q.Bitmap()
	var sb strings.Builder
	for _, row := range bm {
		for _, cell := range row {
			if cell {
				sb.WriteString("██")
			} else {
				sb.WriteString("  ")
			}
		}
		sb.WriteByte('\n')
	}
	fmt.Print(sb.String())
}
