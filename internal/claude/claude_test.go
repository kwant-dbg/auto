package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes a stand-in `claude` executable. Its behaviour is chosen by
// FAKE_MODE; it records its arguments and stdin for assertions.
func fakeClaude(t *testing.T) (bin, logDir string) {
	t.Helper()
	dir := t.TempDir()
	logDir = filepath.Join(dir, "log")
	if err := os.Mkdir(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
LOG="` + logDir + `"
if [ "$1" = "--version" ]; then echo "2.1.306 (Claude Code)"; exit 0; fi
if [ "$1" = "auth" ]; then
  case "$FAKE_AUTH" in
    api) echo '{"loggedIn":true,"authMethod":"api_key"}';;
    none) echo '{"loggedIn":false,"authMethod":"none"}'; exit 1;;
    *) echo '{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty"}';;
  esac
  exit 0
fi
printf '%s\n' "$@" > "$LOG/args"
cat > "$LOG/stdin"
env > "$LOG/env"
case "$FAKE_MODE" in
  ok) echo '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1","num_turns":2,"total_cost_usd":0.01,"usage":{"input_tokens":10},"structured_output":{"answer":42}}';;
  nostruct) echo '{"type":"result","subtype":"success","is_error":false,"result":"no json","num_turns":1}';;
  ratelimit) echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: 429 rate limit exceeded"}'; exit 1;;
  garbage) echo 'not json' >&2; exit 2;;
  slow) trap 'echo "{\"type\":\"result\",\"is_error\":true,\"result\":\"interrupted\"}"; exit 130' INT; sleep 30 & wait;;
esac
`
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logDir
}

func newRunner(t *testing.T, bin string) *Runner {
	t.Helper()
	for _, k := range ForbiddenEnv {
		t.Setenv(k, "")
	}
	r, err := New(Config{Binary: bin, WorkDir: filepath.Join(t.TempDir(), "wd"), VersionPin: "2.1.306", Grace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCheckEnv(t *testing.T) {
	if err := CheckEnv([]string{"PATH=/bin", "ANTHROPIC_API_KEY="}); err != nil {
		t.Fatalf("empty value should be allowed: %v", err)
	}
	err := CheckEnv([]string{"ANTHROPIC_API_KEY=sk-x", "CLAUDE_CODE_USE_BEDROCK=1"})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || !strings.Contains(err.Error(), "CLAUDE_CODE_USE_BEDROCK") {
		t.Fatalf("CheckEnv = %v", err)
	}
}

func TestNewRefusesAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if _, err := New(Config{WorkDir: t.TempDir()}); err == nil {
		t.Fatal("New must refuse to start with ANTHROPIC_API_KEY set")
	}
}

func TestArgs(t *testing.T) {
	bin, _ := fakeClaude(t)
	r := newRunner(t, bin)
	args := r.Args(Spec{Model: "haiku", Schema: json.RawMessage(`{"type":"object"}`), MaxTurns: 3})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p", "--output-format json", "--permission-mode dontAsk", "--no-session-persistence",
		"--strict-mcp-config", "--model haiku", "--max-turns 3", `--json-schema {"type":"object"}`} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	// No tools by default: --tools "" disables every built-in.
	i := slices.Index(args, "--tools")
	if i < 0 || args[i+1] != "" {
		t.Errorf("--tools should be empty by default: %v", args)
	}
	if slices.Contains(args, "--allowedTools") {
		t.Errorf("no tools should be pre-approved by default: %v", args)
	}

	agent := r.Args(Spec{Tools: []string{"WebSearch"}, AllowedTools: []string{"WebSearch", "mcp__aoe__kb_search"}})
	j := strings.Join(agent, " ")
	if !strings.Contains(j, "--tools WebSearch") || !strings.Contains(j, "--allowedTools WebSearch,mcp__aoe__kb_search") {
		t.Errorf("agent args: %s", j)
	}
}

func TestRunStructuredOutput(t *testing.T) {
	bin, logDir := fakeClaude(t)
	r := newRunner(t, bin)
	t.Setenv("FAKE_MODE", "ok")
	res, err := r.Run(context.Background(), Spec{Prompt: "extract this", Schema: json.RawMessage(`{"type":"object"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Structured) != `{"answer":42}` || res.NumTurns != 2 || res.SessionID != "s1" || res.CostUSD != 0.01 {
		t.Fatalf("result = %+v", res)
	}
	if res.Status(err) != "ok" {
		t.Fatalf("status = %s", res.Status(err))
	}
	stdin, _ := os.ReadFile(filepath.Join(logDir, "stdin"))
	if string(stdin) != "extract this" {
		t.Fatalf("prompt not sent on stdin: %q", stdin)
	}
	env, _ := os.ReadFile(filepath.Join(logDir, "env"))
	if !strings.Contains(string(env), "CLAUDE_CODE_DISABLE_WEB_FETCH=1") {
		t.Fatal("WebFetch must be disabled for every call")
	}
}

func TestRunMissingStructuredOutput(t *testing.T) {
	bin, _ := fakeClaude(t)
	r := newRunner(t, bin)
	t.Setenv("FAKE_MODE", "nostruct")
	res, err := r.Run(context.Background(), Spec{Schema: json.RawMessage(`{"type":"object"}`)})
	if !errors.Is(err, ErrInvalidOutput) || res.Status(err) != "invalid_output" {
		t.Fatalf("err = %v status = %s", err, res.Status(err))
	}
	// Without a schema, plain text is fine.
	if _, err := r.Run(context.Background(), Spec{}); err != nil {
		t.Fatalf("schemaless run: %v", err)
	}
}

func TestRunRateLimited(t *testing.T) {
	bin, _ := fakeClaude(t)
	r := newRunner(t, bin)
	t.Setenv("FAKE_MODE", "ratelimit")
	_, err := r.Run(context.Background(), Spec{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestRunUnparseableFailure(t *testing.T) {
	bin, _ := fakeClaude(t)
	r := newRunner(t, bin)
	t.Setenv("FAKE_MODE", "garbage")
	res, err := r.Run(context.Background(), Spec{})
	if err == nil || res.ExitCode != 2 || !strings.Contains(err.Error(), "not json") {
		t.Fatalf("err = %v exit = %d", err, res.ExitCode)
	}
}

func TestRunTimeoutSendsInterrupt(t *testing.T) {
	bin, _ := fakeClaude(t)
	r := newRunner(t, bin)
	t.Setenv("FAKE_MODE", "slow")
	start := time.Now()
	res, err := r.Run(context.Background(), Spec{Timeout: 300 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) || res.Status(err) != "timeout" {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %s; the subprocess was not stopped", time.Since(start))
	}
}

func TestCheck(t *testing.T) {
	bin, _ := fakeClaude(t)
	r := newRunner(t, bin)

	h, err := r.Check(context.Background())
	if err != nil || h.Version != "2.1.306" || h.AuthMethod != "oauth_token" || !h.LoggedIn {
		t.Fatalf("check = %+v, %v", h, err)
	}

	t.Setenv("FAKE_AUTH", "api")
	if _, err := r.Check(context.Background()); err == nil {
		t.Fatal("API-key auth must fail the check")
	}
	t.Setenv("FAKE_AUTH", "none")
	if _, err := r.Check(context.Background()); err == nil {
		t.Fatal("logged-out CLI must fail the check")
	}

	t.Setenv("FAKE_AUTH", "")
	r.cfg.VersionPin = "9.9.9"
	if _, err := r.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("version mismatch: %v", err)
	}
}

func TestRelativeWorkDirBecomesAbsolute(t *testing.T) {
	bin, _ := fakeClaude(t)
	for _, k := range ForbiddenEnv {
		t.Setenv(k, "")
	}
	t.Chdir(t.TempDir())
	r, err := New(Config{Binary: bin, WorkDir: "data/wd"})
	if err != nil {
		t.Fatal(err)
	}
	args := r.Args(Spec{})
	i := slices.Index(args, "--mcp-config")
	if !filepath.IsAbs(args[i+1]) {
		t.Fatalf("--mcp-config %q is relative; calls run inside the workdir", args[i+1])
	}
	path, err := r.WriteMCPConfig("/usr/bin/aoe-mcp", nil)
	if err != nil || !filepath.IsAbs(path) {
		t.Fatalf("mcp config path %q err %v", path, err)
	}
}
