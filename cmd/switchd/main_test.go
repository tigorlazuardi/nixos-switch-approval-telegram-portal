package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAllowed(t *testing.T) {
	ids, err := parseAllowed("123, 456")
	if err != nil {
		t.Fatal(err)
	}
	if !ids[123] || !ids[456] || ids[789] {
		t.Fatalf("unexpected ids: %#v", ids)
	}
	if _, err := parseAllowed("nope"); err == nil {
		t.Fatal("expected invalid id error")
	}
}

func TestReadEnvFileWins(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret")
	if err := os.WriteFile(p, []byte("from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWITCHD_TEST_VALUE", "plain")
	t.Setenv("SWITCHD_TEST_VALUE_FILE", p)
	if got := readEnvFile("SWITCHD_TEST_VALUE", ""); got != "from-file" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadConfigRequiresRepoDir(t *testing.T) {
	t.Setenv("SWITCHD_BOT_TOKEN", "token")
	t.Setenv("SWITCHD_ALLOWED_USER_IDS", "123")
	t.Setenv("SWITCHD_CHAT_ID", "456")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "SWITCHD_REPO_DIR or SWITCHD_REPO_DIR_FILE is required") {
		t.Fatalf("expected repo dir required error, got %v", err)
	}
}

func TestLoadConfigDefaultsFlakeFromRequiredRepoDir(t *testing.T) {
	t.Setenv("SWITCHD_BOT_TOKEN", "token")
	t.Setenv("SWITCHD_ALLOWED_USER_IDS", "123")
	t.Setenv("SWITCHD_CHAT_ID", "456")
	t.Setenv("SWITCHD_REPO_DIR", "/repo")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FlakeRef != "/repo#homeserver" {
		t.Fatalf("flake ref = %q", cfg.FlakeRef)
	}
}

func TestLoadConfigRepoDirAndFlakeRefFile(t *testing.T) {
	dir := t.TempDir()
	repoFile := filepath.Join(dir, "repo")
	flakeFile := filepath.Join(dir, "flake")
	if err := os.WriteFile(repoFile, []byte("/private/repo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flakeFile, []byte("/private/repo#host\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWITCHD_BOT_TOKEN", "token")
	t.Setenv("SWITCHD_ALLOWED_USER_IDS", "123")
	t.Setenv("SWITCHD_CHAT_ID", "456")
	t.Setenv("SWITCHD_REPO_DIR", "/visible/repo")
	t.Setenv("SWITCHD_REPO_DIR_FILE", repoFile)
	t.Setenv("SWITCHD_FLAKE_REF_FILE", flakeFile)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepoDir != "/private/repo" || cfg.FlakeRef != "/private/repo#host" {
		t.Fatalf("repo/flake = %q %q", cfg.RepoDir, cfg.FlakeRef)
	}
}

func TestNonceUnique(t *testing.T) {
	a, err := nonce()
	if err != nil {
		t.Fatal(err)
	}
	b, err := nonce()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 || len(b) != 32 || a == b {
		t.Fatalf("bad nonces %q %q", a, b)
	}
}

func TestConsumeOneShot(t *testing.T) {
	st := &requestState{id: "abc"}
	d := &daemon{current: st}
	if got := d.consume("abc"); got != st {
		t.Fatalf("first consume = %#v", got)
	}
	if got := d.consume("abc"); got != nil {
		t.Fatalf("second consume = %#v", got)
	}
}

func TestActivationContextZeroHasNoDeadline(t *testing.T) {
	ctx, cancel := activationContext(0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("zero activation timeout should not set a deadline")
	}
}

func TestActivationContextNonZeroHasDeadline(t *testing.T) {
	ctx, cancel := activationContext(time.Minute)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("non-zero activation timeout should set a deadline")
	}
}

func TestFormatApprovalMessageUnderTelegramLimit(t *testing.T) {
	st := &requestState{id: strings.Repeat("a", 64), mode: modeSync, reason: strings.Repeat("<&reason>", 200), toplevelPath: "/nix/store/" + strings.Repeat("toplevel", 80)}
	msg := formatApprovalMessage(st, strings.Repeat("commit <x>\n", 500), strings.Repeat(" M dirty-file\n", 500), strings.Repeat("diff <pkg>\n", 5000))
	if len(msg) > approvalMessageMax {
		t.Fatalf("message length %d exceeds cap %d", len(msg), approvalMessageMax)
	}
	for _, want := range []string{"Request:", "Mode:", "Toplevel:", "Recent commits", "Dirty files", "nvd diff"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q: %s", want, msg)
		}
	}
}

func TestEscapedTrimStaysBounded(t *testing.T) {
	for _, input := range []string{"<&é", strings.Repeat("<", 100), strings.Repeat("&", 100), strings.Repeat("é", 100)} {
		for n := 0; n <= 40; n++ {
			got := escapedTrim(input, n)
			if len(got) > n {
				t.Fatalf("escapedTrim(%q, %d) length = %d: %q", input, n, len(got), got)
			}
			if strings.ContainsAny(got, "<>") {
				t.Fatalf("escapedTrim(%q, %d) returned unescaped HTML: %q", input, n, got)
			}
		}
	}
}

func TestSanitizedBuildRefExcludesGitMetadataAndKeepsDirtyContent(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "flake.nix"), []byte("dirty untracked"), 0644); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(source, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects", "info"), 0755); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{
		"config":                  "[include]\npath=/tmp/evil\n[core]\nworktree=/tmp/worktree\nfsmonitor=/tmp/helper\n",
		"objects/info/alternates": "/tmp/objects\n",
	}
	for name, content := range metadata {
		if err := os.WriteFile(filepath.Join(gitDir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	ref, cleanup, err := sanitizedBuildRef("git+file://" + source + "#homeserver")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasPrefix(ref, "path:") || !strings.HasSuffix(ref, "#homeserver") {
		t.Fatalf("local ref was not rewritten to path semantics: %q", ref)
	}
	snapshot := strings.TrimSuffix(strings.TrimPrefix(ref, "path:"), "#homeserver")
	if _, err := os.Lstat(filepath.Join(snapshot, ".git")); !os.IsNotExist(err) {
		t.Fatalf("Git metadata copied into snapshot: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(snapshot, "flake.nix"))
	if err != nil || string(got) != "dirty untracked" {
		t.Fatalf("dirty content missing: %q, %v", got, err)
	}
}

func TestSanitizedBuildRefLeavesRemoteRefNative(t *testing.T) {
	const remote = "github:example/flake?rev=abc#host"
	got, cleanup, err := sanitizedBuildRef(remote)
	defer cleanup()
	if err != nil || got != remote {
		t.Fatalf("remote ref = %q, %v", got, err)
	}
}

func TestCopySanitizedTreeRejectsSpecialFile(t *testing.T) {
	source := t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := copySanitizedTree(source, filepath.Join(t.TempDir(), "snapshot")); err == nil || !strings.Contains(err.Error(), "escapes source tree") {
		t.Fatalf("expected escaping symlink rejection, got %v", err)
	}
}

func TestCopySanitizedTreeReportsDirectoryEnumerationPermission(t *testing.T) {
	requirePermissionEnforcement(t)
	source := t.TempDir()
	locked := filepath.Join(source, "locked")
	if err := os.Mkdir(locked, 0111); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0700)

	err := copySanitizedTree(source, filepath.Join(t.TempDir(), "snapshot"))
	assertPermissionError(t, err, locked, permissionEnumerate)
}

func TestCopySanitizedTreeReportsRegularFileReadPermission(t *testing.T) {
	requirePermissionEnforcement(t)
	source := t.TempDir()
	locked := filepath.Join(source, "locked")
	if err := os.WriteFile(locked, []byte("secret"), 0000); err != nil {
		t.Fatal(err)
	}

	err := copySanitizedTree(source, filepath.Join(t.TempDir(), "snapshot"))
	assertPermissionError(t, err, locked, permissionRead)
}

func TestCopySanitizedTreeReportsSymlinkResolutionPermission(t *testing.T) {
	requirePermissionEnforcement(t)
	outside := t.TempDir()
	locked := filepath.Join(outside, "locked")
	if err := os.Mkdir(locked, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0700)
	source := t.TempDir()
	link := filepath.Join(source, "link")
	if err := os.Symlink(filepath.Join(locked, "target"), link); err != nil {
		t.Fatal(err)
	}

	err := copySanitizedTree(source, filepath.Join(t.TempDir(), "snapshot"))
	assertPermissionError(t, err, filepath.Join(locked, "target"), permissionResolve)
	if strings.Contains(err.Error(), "dangling symlink") {
		t.Fatalf("permission failure mislabeled as dangling symlink: %v", err)
	}
}

func requirePermissionEnforcement(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode-bit permission boundaries")
	}
}

func assertPermissionError(t *testing.T, err error, path string, operation permissionOperation) {
	t.Helper()
	if err == nil || !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), string(operation)) {
		t.Fatalf("permission error = %v; want path %q and operation %q", err, path, operation)
	}
}

func TestResolveBuiltToplevelRejectsNonStoreResult(t *testing.T) {
	dir := t.TempDir()
	built := filepath.Join(dir, "built")
	if err := os.MkdirAll(filepath.Join(built, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(built, "bin", "switch-to-configuration"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(built, filepath.Join(dir, "result")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveBuiltToplevel(dir); err == nil || !strings.Contains(err.Error(), "not under /nix/store/") {
		t.Fatalf("expected non-store error, got %v", err)
	}
}

func TestResolveBuiltToplevelRejectsMissingResult(t *testing.T) {
	if _, err := resolveBuiltToplevel(t.TempDir()); err == nil || !strings.Contains(err.Error(), "resolve result symlink") {
		t.Fatalf("expected resolve error, got %v", err)
	}
}

func TestTelegramTransportErrorRedactsToken(t *testing.T) {
	const token = "123456:SECRET"
	tg := newTelegram(token)
	tg.c = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})}
	err := tg.sendMessage("chat", "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	s := err.Error()
	if strings.Contains(s, token) || strings.Contains(s, "/bot"+token+"/") || strings.Contains(s, "sendMessage?token") {
		t.Fatalf("telegram error leaked token: %q", s)
	}
	if !strings.Contains(s, "telegram sendMessage transport") || !strings.Contains(s, "connection refused") {
		t.Fatalf("telegram error lost method/cause: %q", s)
	}
}

func TestRunRequestFailureEndsWithDone(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "not-a-dir", "child")
	if err := os.WriteFile(filepath.Dir(logDir), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &daemon{cfg: config{LogDir: logDir}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st := &requestState{id: "abc", mode: modeSync, stream: make(chan frame, 64), done: make(chan int, 1)}
	d.runRequest(st)

	var frames []frame
	for f := range st.stream {
		frames = append(frames, f)
	}
	if len(frames) < 2 || frames[len(frames)-1].Type != "done" || frames[len(frames)-1].ExitCode != 1 {
		t.Fatalf("missing terminal done frame: %#v", frames)
	}
	if frames[0].Type != "error" {
		t.Fatalf("expected error before done: %#v", frames)
	}
}

func TestHandleConnInvalidRequestEndsWithDone(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		(&daemon{}).handleConn(server)
		close(done)
	}()
	if _, err := client.Write([]byte("{bad json}\n")); err != nil {
		t.Fatal(err)
	}

	dec := json.NewDecoder(client)
	var frames [2]frame
	for i := range frames {
		if err := dec.Decode(&frames[i]); err != nil {
			t.Fatal(err)
		}
	}
	if frames[0].Type != "error" || frames[0].ExitCode != 2 {
		t.Fatalf("first frame = %#v", frames[0])
	}
	if frames[1].Type != "done" || frames[1].ExitCode != 2 {
		t.Fatalf("second frame = %#v", frames[1])
	}
	<-done
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
