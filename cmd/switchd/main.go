// Command switchd is the switch-approval daemon. It accepts fixed-shape switch
// requests over a Unix socket, asks an allow-listed Telegram user to approve a
// nonce-bound request, then runs fixed argv-vector Nix build and activation commands.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	modeSync  = "sync"
	modeAsync = "async"

	approvalMessageMax = 3900
)

type config struct {
	BotToken        string
	AllowedUserIDs  map[int64]bool
	ChatID          string
	SocketPath      string
	RepoDir         string
	FlakeRef        string
	SyncTimeout     time.Duration
	AsyncTimeout    time.Duration
	ActivateTimeout time.Duration
	ActivateCmd     []string
	MetricsAddr     string
	LogDir          string
}

type clientRequest struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}

type frame struct {
	Type     string `json:"type"`
	Message  string `json:"message,omitempty"`
	Line     string `json:"line,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

type daemon struct {
	cfg     config
	log     *slog.Logger
	tg      *telegram
	metrics metrics

	mu      sync.Mutex
	current *requestState
}

type requestState struct {
	id           string
	mode         string
	reason       string
	timeout      time.Duration
	stream       chan frame
	approved     chan bool
	done         chan int
	started      time.Time
	logPath      string
	toplevelPath string
	msgID        int
	consumed     bool
	logTail      []string
}

type metrics struct {
	requests sync.Map // key mode|outcome -> *atomic.Int64
	pending  atomic.Int64
}

func main() {
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig()
	if err != nil {
		logger.Error("invalid config", "error", err)
		os.Exit(1)
	}
	d := &daemon{cfg: cfg, log: logger, tg: newTelegram(cfg.BotToken), metrics: metrics{}}

	ctx, cancel := signalContext()
	defer cancel()
	if cfg.MetricsAddr != "" {
		go d.serveMetrics(ctx)
	}
	go d.pollTelegram(ctx)
	if err := d.serveSocket(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("socket server stopped", "error", err)
		os.Exit(1)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 2)
	signalNotify(c)
	go func() { <-c; cancel() }()
	return ctx, cancel
}

var signalNotify = func(c chan<- os.Signal) { signal.Notify(c, syscall.SIGINT, syscall.SIGTERM) }

func loadConfig() (config, error) {
	repo := strings.TrimSpace(readEnvFile("SWITCHD_REPO_DIR", ""))
	if repo == "" {
		return config{}, fmt.Errorf("SWITCHD_REPO_DIR or SWITCHD_REPO_DIR_FILE is required")
	}
	flake := readEnvFile("SWITCHD_FLAKE_REF", repo+"#homeserver")
	allowed, err := parseAllowed(readEnvFile("SWITCHD_ALLOWED_USER_IDS", ""))
	if err != nil {
		return config{}, err
	}
	activate := strings.Fields(env("SWITCHD_ACTIVATE_CMD", "sudo"))
	if len(activate) == 0 {
		return config{}, fmt.Errorf("SWITCHD_ACTIVATE_CMD is empty")
	}
	cfg := config{
		BotToken:        strings.TrimSpace(readEnvFile("SWITCHD_BOT_TOKEN", "")),
		AllowedUserIDs:  allowed,
		ChatID:          strings.TrimSpace(readEnvFile("SWITCHD_CHAT_ID", "")),
		SocketPath:      env("SWITCHD_SOCKET_PATH", "/run/switchd/sock"),
		RepoDir:         repo,
		FlakeRef:        flake,
		SyncTimeout:     durationEnv("SWITCHD_SYNC_TIMEOUT", 30*time.Minute),
		AsyncTimeout:    durationEnv("SWITCHD_ASYNC_TIMEOUT", 24*time.Hour),
		ActivateTimeout: durationEnv("SWITCHD_ACTIVATE_TIMEOUT", 30*time.Minute),
		ActivateCmd:     activate,
		MetricsAddr:     env("SWITCHD_METRICS_ADDR", "127.0.0.1:9464"),
		LogDir:          env("SWITCHD_LOG_DIR", "/var/log/switchd"),
	}
	if cfg.BotToken == "" {
		return config{}, fmt.Errorf("SWITCHD_BOT_TOKEN or SWITCHD_BOT_TOKEN_FILE is required")
	}
	if len(cfg.AllowedUserIDs) == 0 {
		return config{}, fmt.Errorf("SWITCHD_ALLOWED_USER_IDS or SWITCHD_ALLOWED_USER_IDS_FILE is required")
	}
	if cfg.ChatID == "" {
		return config{}, fmt.Errorf("SWITCHD_CHAT_ID or SWITCHD_CHAT_ID_FILE is required")
	}
	return cfg, nil
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func readEnvFile(name, def string) string {
	if p := os.Getenv(name + "_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	return env(name, def)
}

func durationEnv(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err == nil {
		return d
	}
	if minutes, err := strconv.Atoi(v); err == nil {
		return time.Duration(minutes) * time.Minute
	}
	return def
}

func parseAllowed(s string) (map[int64]bool, error) {
	ids := map[int64]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed Telegram user id %q: %w", part, err)
		}
		ids[id] = true
	}
	return ids, nil
}

func (d *daemon) serveSocket(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(d.cfg.SocketPath), 0750); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	_ = os.Remove(d.cfg.SocketPath)
	oldUmask := syscall.Umask(0077)
	ln, err := net.Listen("unix", d.cfg.SocketPath)
	syscall.Umask(oldUmask)
	if err != nil {
		return fmt.Errorf("listen unix socket: %w", err)
	}
	defer ln.Close()
	if err := os.Chmod(d.cfg.SocketPath, 0660); err != nil {
		return fmt.Errorf("chmod unix socket: %w", err)
	}
	go func() { <-ctx.Done(); _ = ln.Close(); _ = os.Remove(d.cfg.SocketPath) }()
	d.log.Info("socket listening", "path", d.cfg.SocketPath)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accept: %w", err)
		}
		go d.handleConn(conn)
	}
}

func (d *daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	var req clientRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		writeFrame(conn, frame{Type: "error", Message: "invalid request", ExitCode: 2})
		writeFrame(conn, frame{Type: "done", ExitCode: 2})
		return
	}
	stream, accepted, code := d.startRequest(req)
	if !accepted {
		writeFrame(conn, frame{Type: "status", Message: "busy", ExitCode: code})
		writeFrame(conn, frame{Type: "done", ExitCode: code})
		return
	}
	if req.Mode == modeAsync {
		writeFrame(conn, frame{Type: "status", Message: "accepted", ExitCode: 0})
		writeFrame(conn, frame{Type: "done", ExitCode: 0})
		return
	}
	enc := json.NewEncoder(conn)
	for f := range stream {
		if err := enc.Encode(f); err != nil {
			return
		}
	}
}

func (d *daemon) startRequest(req clientRequest) (<-chan frame, bool, int) {
	if req.Mode == "" {
		req.Mode = modeSync
	}
	if req.Mode != modeSync && req.Mode != modeAsync {
		return nil, false, 2
	}
	id, err := nonce()
	if err != nil {
		d.log.Error("nonce failed", "error", err)
		return nil, false, 1
	}
	st := &requestState{id: id, mode: req.Mode, reason: req.Reason, approved: make(chan bool, 1), done: make(chan int, 1), started: time.Now(), stream: make(chan frame, 64)}
	if req.Mode == modeSync {
		st.timeout = d.cfg.SyncTimeout
	} else {
		st.timeout = d.cfg.AsyncTimeout
	}
	d.mu.Lock()
	if d.current != nil {
		d.mu.Unlock()
		d.count(req.Mode, "busy")
		return nil, false, 75
	}
	d.current = st
	d.metrics.pending.Store(1)
	d.mu.Unlock()

	d.count(req.Mode, "started")
	d.log.Info("request accepted", "request_id", id, "mode", req.Mode, "reason", req.Reason)
	go d.runRequest(st)
	return st.stream, true, 0
}

func (d *daemon) runRequest(st *requestState) {
	outcome := "failed"
	code := 1
	doneMessage := ""
	defer func() {
		d.finish(st, outcome)
		st.done <- code
		if st.mode == modeSync {
			d.emitFinal(st, frame{Type: "done", Message: doneMessage, ExitCode: code})
		}
		close(st.stream)
		d.count(st.mode, outcome)
		d.log.Info("request finished", "request_id", st.id, "mode", st.mode, "outcome", outcome, "exit_code", code)
	}()

	logFile, err := d.openLog(st)
	if err != nil {
		d.emit(st, frame{Type: "error", Message: err.Error()})
		return
	}
	defer logFile.Close()
	st.logPath = logFile.Name()
	preApprovalCtx, cancelPreApproval := context.WithTimeout(context.Background(), st.timeout)
	defer cancelPreApproval()

	d.emit(st, frame{Type: "status", Message: "building"})
	start := time.Now()
	buildRef, cleanupSnapshot, err := sanitizedBuildRef(d.cfg.FlakeRef)
	if err != nil {
		d.emit(st, frame{Type: "error", Message: "prepare build source: " + err.Error()})
		return
	}
	defer cleanupSnapshot()
	buildDir, err := os.MkdirTemp("", "switchd-build-")
	if err != nil {
		d.emit(st, frame{Type: "error", Message: "prepare build output: " + err.Error()})
		return
	}
	defer os.RemoveAll(buildDir)
	outLink := filepath.Join(buildDir, "result")
	buildArgv, err := toplevelBuildArgv(buildRef, outLink)
	if err != nil {
		d.emit(st, frame{Type: "error", Message: "prepare build installable: " + err.Error()})
		return
	}
	if err := d.runCmd(preApprovalCtx, st, logFile, buildArgv); err != nil {
		d.emit(st, frame{Type: "error", Message: "build failed: " + err.Error()})
		d.notify(st, "Build failed", tail(st.logTail, 25))
		return
	}
	st.toplevelPath, err = resolveBuiltToplevel(outLink)
	if err != nil {
		d.emit(st, frame{Type: "error", Message: "built toplevel invalid: " + err.Error()})
		d.notify(st, "Build produced invalid toplevel", []string{err.Error()})
		return
	}
	msg, err := d.approvalMessage(st)
	if err != nil {
		d.emit(st, frame{Type: "error", Message: err.Error()})
		return
	}
	msgID, err := d.tg.sendApproval(d.cfg.ChatID, msg, st.id)
	if err != nil {
		d.emit(st, frame{Type: "error", Message: "telegram send failed: " + err.Error()})
		return
	}
	st.msgID = msgID
	d.emit(st, frame{Type: "status", Message: "awaiting approval"})

	select {
	case ok := <-st.approved:
		if !ok {
			outcome, code, doneMessage = "rejected", 3, "rejected"
			d.notify(st, "Rejected", nil)
			return
		}
	case <-preApprovalCtx.Done():
		outcome, code, doneMessage = "expired", 4, "expired"
		d.consume(st.id)
		d.notify(st, "Expired", nil)
		return
	}

	d.emit(st, frame{Type: "status", Message: "approved; activating"})
	activateCtx, cancelActivate := activationContext(d.cfg.ActivateTimeout)
	defer cancelActivate()
	argv := append(append([]string{}, d.cfg.ActivateCmd...), filepath.Join(st.toplevelPath, "bin/switch-to-configuration"), "switch")
	if err := d.runCmd(activateCtx, st, logFile, argv); err != nil {
		d.emit(st, frame{Type: "error", Message: "activation failed: " + err.Error()})
		d.notify(st, "Activation failed", tail(st.logTail, 40))
		return
	}
	outcome, code, doneMessage = "approved", 0, "switch complete"
	d.observeDuration(time.Since(start))
	d.notify(st, "Switch complete", []string{"log: " + st.logPath})
}

func (d *daemon) openLog(st *requestState) (*os.File, error) {
	if err := os.MkdirAll(d.cfg.LogDir, 0750); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	return os.Create(filepath.Join(d.cfg.LogDir, time.Now().Format("20060102-150405-")+st.id+".log"))
}

func (d *daemon) runCmd(ctx context.Context, st *requestState, logFile io.Writer, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = d.cfg.RepoDir
	cmd.Env = os.Environ()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", argv[0], err)
	}
	s := bufio.NewScanner(out)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := s.Text()
		_, _ = fmt.Fprintln(logFile, line)
		st.logTail = append(tail(st.logTail, 199), line)
		d.emit(st, frame{Type: "log", Line: line})
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("read %s output: %w", argv[0], err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s failed: %w", argv[0], err)
	}
	return nil
}

func toplevelBuildArgv(flakeRef, outLink string) ([]string, error) {
	installable, err := toplevelInstallable(flakeRef)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(outLink) || filepath.Clean(outLink) != outLink {
		return nil, fmt.Errorf("out-link must be a canonical absolute path")
	}
	return []string{"nix", "build", "--out-link", outLink, installable}, nil
}

func toplevelInstallable(flakeRef string) (string, error) {
	if !utf8.ValidString(flakeRef) {
		return "", fmt.Errorf("flake ref must be valid UTF-8")
	}
	if strings.Count(flakeRef, "#") != 1 {
		return "", fmt.Errorf("flake ref must contain exactly one fragment separator")
	}
	base, fragment, _ := strings.Cut(flakeRef, "#")
	if base == "" || fragment == "" {
		return "", fmt.Errorf("flake ref and fragment must both be non-empty")
	}
	if strings.HasPrefix(base, "-") {
		return "", fmt.Errorf("flake ref must not begin with an option prefix")
	}
	var quoted strings.Builder
	for i, r := range fragment {
		if r < ' ' || r == '\u007f' {
			return "", fmt.Errorf("flake fragment %q contains unsupported characters", fragment)
		}
		if r == '"' || r == '\\' || r == '$' && i+1 < len(fragment) && fragment[i+1] == '{' {
			quoted.WriteByte('\\')
		}
		quoted.WriteRune(r)
	}
	return base + `#nixosConfigurations."` + quoted.String() + `".config.system.build.toplevel`, nil
}

func sanitizedBuildRef(ref string) (string, func(), error) {
	if _, err := toplevelInstallable(ref); err != nil {
		return "", func() {}, err
	}
	path, fragment, local, err := localFlakeRef(ref)
	if err != nil || !local {
		return ref, func() {}, err
	}
	tmp, err := os.MkdirTemp("", "switchd-source-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create source snapshot: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	snapshot := filepath.Join(tmp, "source")
	if err := copySanitizedTree(path, snapshot); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return "path:" + snapshot + fragment, cleanup, nil
}

func localFlakeRef(ref string) (path, fragment string, local bool, err error) {
	pathPart, fragment, _ := strings.Cut(ref, "#")
	if strings.Contains(pathPart, "?") {
		switch {
		case strings.HasPrefix(pathPart, "/"), strings.HasPrefix(pathPart, "path:/"), strings.HasPrefix(pathPart, "file:///"), strings.HasPrefix(pathPart, "git+file:///"):
			return "", "", false, fmt.Errorf("local flake refs must not contain query parameters")
		}
	}
	switch {
	case strings.HasPrefix(pathPart, "/"):
		path = pathPart
	case strings.HasPrefix(pathPart, "path:/"):
		path = strings.TrimPrefix(pathPart, "path:")
	case strings.HasPrefix(pathPart, "file:///"):
		path = "/" + strings.TrimPrefix(pathPart, "file:///")
	case strings.HasPrefix(pathPart, "git+file:///"):
		path = "/" + strings.TrimPrefix(pathPart, "git+file:///")
	default:
		return "", "", false, nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", false, fmt.Errorf("local flake path must be canonical and absolute")
	}
	resolved, resolveErr := filepath.EvalSymlinks(path)
	if resolveErr != nil {
		if errors.Is(resolveErr, os.ErrPermission) {
			return "", "", false, sourcePermissionError(path, permissionResolve, resolveErr)
		}
		return "", "", false, fmt.Errorf("local flake path must exist and contain no symlinks: %w", resolveErr)
	}
	if resolved != path {
		return "", "", false, fmt.Errorf("local flake path must exist and contain no symlinks")
	}
	if fragment != "" {
		fragment = "#" + fragment
	}
	return path, fragment, true, nil
}

func copySanitizedTree(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return sourcePermissionError(source, permissionResolve, err)
		}
		return fmt.Errorf("inspect local flake: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local flake source is not a directory")
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return fmt.Errorf("create source snapshot: %w", err)
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrPermission) {
				return sourcePermissionError(path, permissionEnumerate, walkErr)
			}
			return walkErr
		}
		if path == source {
			return nil
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(destination, rel)
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrPermission) {
				return sourcePermissionError(path, permissionResolve, err)
			}
			return err
		}
		switch {
		case info.IsDir():
			if err := os.Mkdir(dst, 0700); err != nil {
				return err
			}
			return os.Chmod(dst, info.Mode().Perm()|0700)
		case info.Mode().IsRegular():
			return copyRegularFile(path, dst, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				if errors.Is(err, os.ErrPermission) {
					return sourcePermissionError(path, permissionResolve, err)
				}
				return fmt.Errorf("local flake contains dangling symlink %q: %w", path, err)
			}
			resolved, err = filepath.Abs(resolved)
			if err != nil {
				return err
			}
			if resolved != source && !strings.HasPrefix(resolved, source+string(filepath.Separator)) && !strings.HasPrefix(resolved, "/nix/store/") {
				return fmt.Errorf("local flake symlink %q escapes source tree and Nix store", path)
			}
			return os.Symlink(target, dst)
		default:
			return fmt.Errorf("local flake contains unsupported special file %q", path)
		}
	})
}

type permissionOperation string

const (
	permissionResolve   permissionOperation = "source and ancestor directories need execute/traverse permission"
	permissionEnumerate permissionOperation = "directory needs read and execute/traverse permission"
	permissionRead      permissionOperation = "regular file needs read permission"
)

func sourcePermissionError(path string, operation permissionOperation, err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Path != "" {
		path = pathErr.Path
	}
	return fmt.Errorf("cannot access local flake path %q: %s: %w", path, operation, err)
}

func copyRegularFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return sourcePermissionError(source, permissionRead, err)
		}
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func resolveBuiltToplevel(outLink string) (string, error) {
	info, err := os.Lstat(outLink)
	if err != nil {
		return "", fmt.Errorf("inspect result out-link: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("result out-link %q is not a symlink", outLink)
	}
	path, err := filepath.EvalSymlinks(outLink)
	if err != nil {
		return "", fmt.Errorf("resolve result out-link: %w", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve result absolute path: %w", err)
	}
	if !strings.HasPrefix(path, "/nix/store/") {
		return "", fmt.Errorf("resolved result %q is not under /nix/store/", path)
	}
	switchToConfiguration := filepath.Join(path, "bin/switch-to-configuration")
	info, err = os.Stat(switchToConfiguration)
	if err != nil {
		return "", fmt.Errorf("missing switch-to-configuration at %q: %w", switchToConfiguration, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("switch-to-configuration at %q is a directory", switchToConfiguration)
	}
	return path, nil
}

func activationContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout == 0 {
		return context.Background(), func() {}
	}
	return context.WithTimeout(context.Background(), timeout)
}

func (d *daemon) approvalMessage(st *requestState) (string, error) {
	if st.toplevelPath == "" {
		return "", fmt.Errorf("built toplevel path is empty")
	}
	gitLog := commandOutput(d.cfg.RepoDir, "git", "log", "--oneline", "-5")
	gitStatus := commandOutput(d.cfg.RepoDir, "git", "status", "--porcelain")
	diff := commandOutput(d.cfg.RepoDir, "nvd", "diff", "/run/current-system", st.toplevelPath)
	return formatApprovalMessage(st, gitLog, gitStatus, diff), nil
}

func formatApprovalMessage(st *requestState, gitLog, gitStatus, diff string) string {
	if strings.TrimSpace(diff) == "" {
		diff = "(nvd produced no diff output)"
	}
	msg := fmt.Sprintf("<b>NixOS switch request</b>\nRequest: <code>%s</code>\nMode: <code>%s</code>\nToplevel: <code>%s</code>\nReason:\n%s\n\n<b>Recent commits</b>\n<pre>%s</pre>\n<b>Dirty files</b>\n<pre>%s</pre>\n<b>nvd diff</b>\n<pre>%s</pre>",
		escapedTrim(st.id, 80), escapedTrim(st.mode, 20), escapedTrim(st.toplevelPath, 300), escapedTrim(st.reason, 700), escapedTrim(gitLog, 800), escapedTrim(empty(gitStatus, "(clean)"), 500), escapedTrim(diff, 1200))
	if len(msg) > approvalMessageMax {
		return msg[:approvalMessageMax]
	}
	return msg
}

func escapedTrim(s string, n int) string {
	if n <= 0 {
		return ""
	}
	escapedAll := html.EscapeString(s)
	if len(escapedAll) <= n {
		return escapedAll
	}

	marker := "\n…truncated…"
	limit := n
	suffix := ""
	if n >= len(marker) {
		limit -= len(marker)
		suffix = marker
	}

	var b strings.Builder
	b.Grow(n)
	for _, r := range s {
		escaped := html.EscapeString(string(r))
		if b.Len()+len(escaped) > limit {
			break
		}
		b.WriteString(escaped)
	}
	return b.String() + suffix
}

func commandOutput(dir string, argv ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(b) + "\n(error: " + err.Error() + ")")
	}
	return strings.TrimSpace(string(b))
}

func (d *daemon) pollTelegram(ctx context.Context) {
	var offset int64
	for ctx.Err() == nil {
		updates, err := d.tg.getUpdates(ctx, offset)
		if err != nil {
			d.log.Warn("telegram poll failed", "error", err)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.CallbackQuery != nil {
				d.handleCallback(u.CallbackQuery)
			}
		}
	}
}

func (d *daemon) handleCallback(cb *callbackQuery) {
	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 || (parts[0] != "approve" && parts[0] != "reject") {
		_ = d.tg.answerCallback(cb.ID, "Invalid action")
		return
	}
	if !d.cfg.AllowedUserIDs[cb.From.ID] {
		d.log.Warn("telegram callback denied", "user_id", cb.From.ID)
		_ = d.tg.answerCallback(cb.ID, "Not allowed")
		return
	}
	st := d.consume(parts[1])
	if st == nil {
		_ = d.tg.answerCallback(cb.ID, "Request expired or already handled")
		return
	}
	approved := parts[0] == "approve"
	_ = d.tg.answerCallback(cb.ID, map[bool]string{true: "Approved", false: "Rejected"}[approved])
	st.approved <- approved
}

func (d *daemon) consume(id string) *requestState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.current == nil || d.current.id != id || d.current.consumed {
		return nil
	}
	d.current.consumed = true
	return d.current
}

func (d *daemon) finish(st *requestState, outcome string) {
	d.mu.Lock()
	if d.current == st {
		d.current = nil
		d.metrics.pending.Store(0)
	}
	d.mu.Unlock()
}

func (d *daemon) emit(st *requestState, f frame) {
	select {
	case st.stream <- f:
	default:
	}
}

func (d *daemon) emitFinal(st *requestState, f frame) {
	select {
	case st.stream <- f:
	default:
		select {
		case <-st.stream:
		default:
		}
		st.stream <- f
	}
}

func (d *daemon) notify(st *requestState, title string, lines []string) {
	body := "<b>" + html.EscapeString(title) + "</b>\nRequest: <code>" + html.EscapeString(st.id) + "</code>\nOutcome log: <code>" + html.EscapeString(st.logPath) + "</code>"
	if len(lines) > 0 {
		body += "\n<pre>" + html.EscapeString(trim(strings.Join(lines, "\n"), 3500)) + "</pre>"
	}
	if err := d.tg.sendMessage(d.cfg.ChatID, body); err != nil {
		d.log.Warn("telegram notify failed", "request_id", st.id, "mode", st.mode, "error", err)
	}
}

func nonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeFrame(w io.Writer, f frame) { _ = json.NewEncoder(w).Encode(f) }

func tail(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…truncated…"
}

func empty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func (d *daemon) count(mode, outcome string) {
	key := mode + "|" + outcome
	v, _ := d.metrics.requests.LoadOrStore(key, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
}

func (d *daemon) observeDuration(time.Duration) {}

func (d *daemon) serveMetrics(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, "# TYPE switchd_pending_requests gauge")
		_, _ = fmt.Fprintf(w, "switchd_pending_requests %d\n", d.metrics.pending.Load())
		_, _ = fmt.Fprintln(w, "# TYPE switchd_requests_total counter")
		d.metrics.requests.Range(func(k, v any) bool {
			parts := strings.Split(k.(string), "|")
			_, _ = fmt.Fprintf(w, "switchd_requests_total{mode=%q,outcome=%q} %d\n", parts[0], parts[1], v.(*atomic.Int64).Load())
			return true
		})
	})
	srv := &http.Server{Addr: d.cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		d.log.Warn("metrics server failed", "error", err)
	}
}

type telegram struct {
	token string
	base  string
	c     *http.Client
}

func newTelegram(token string) *telegram {
	return &telegram{token: token, base: "https://api.telegram.org/bot" + token + "/", c: &http.Client{Timeout: 90 * time.Second}}
}

type update struct {
	UpdateID      int64          `json:"update_id"`
	CallbackQuery *callbackQuery `json:"callback_query"`
}

type callbackQuery struct {
	ID   string `json:"id"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Data string `json:"data"`
}

type tgResponse[T any] struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Result      T      `json:"result"`
}

func (t *telegram) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	v := url.Values{"timeout": {"60"}, "allowed_updates": {`["callback_query"]`}}
	if offset > 0 {
		v.Set("offset", strconv.FormatInt(offset, 10))
	}
	var res tgResponse[[]update]
	if err := t.postForm(ctx, "getUpdates", v, &res); err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, fmt.Errorf("telegram getUpdates: %s", res.Description)
	}
	return res.Result, nil
}

func (t *telegram) sendApproval(chatID, text, id string) (int, error) {
	markup := fmt.Sprintf(`{"inline_keyboard":[[{"text":"Approve","callback_data":"approve:%s"},{"text":"Reject","callback_data":"reject:%s"}]]}`, id, id)
	v := url.Values{"chat_id": {chatID}, "text": {text}, "parse_mode": {"HTML"}, "reply_markup": {markup}}
	var res tgResponse[struct {
		MessageID int `json:"message_id"`
	}]
	if err := t.postForm(context.Background(), "sendMessage", v, &res); err != nil {
		return 0, err
	}
	if !res.OK {
		return 0, fmt.Errorf("telegram sendMessage: %s", res.Description)
	}
	return res.Result.MessageID, nil
}

func (t *telegram) sendMessage(chatID, text string) error {
	v := url.Values{"chat_id": {chatID}, "text": {text}, "parse_mode": {"HTML"}}
	var res tgResponse[json.RawMessage]
	if err := t.postForm(context.Background(), "sendMessage", v, &res); err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("telegram sendMessage: %s", res.Description)
	}
	return nil
}

func (t *telegram) answerCallback(id, text string) error {
	v := url.Values{"callback_query_id": {id}, "text": {text}}
	var res tgResponse[bool]
	if err := t.postForm(context.Background(), "answerCallbackQuery", v, &res); err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("telegram answerCallbackQuery: %s", res.Description)
	}
	return nil
}

func (t *telegram) postForm(ctx context.Context, method string, v url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+method, strings.NewReader(v.Encode()))
	if err != nil {
		return fmt.Errorf("telegram %s request: %s", method, t.sanitizeError(err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.c.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s transport: %s", method, t.sanitizeError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram %s http %s", method, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (t *telegram) sanitizeError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	s := err.Error()
	if t.token != "" {
		s = strings.ReplaceAll(s, t.token, "<redacted>")
	}
	if t.base != "" {
		s = strings.ReplaceAll(s, t.base, "https://api.telegram.org/bot<redacted>/")
	}
	return s
}
