// Package claude runs Claude Code in headless mode (`claude -p`) as a
// subprocess, authenticated with the founder's subscription (docs/04 §7.1).
// It is the only path from AOE to a model.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ForbiddenEnv lists variables that silently take precedence over the
// subscription token and move calls to metered API billing (NFR-11).
var ForbiddenEnv = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
}

// Config holds runner-wide settings.
type Config struct {
	Binary      string        `yaml:"binary"`      // default "claude"
	WorkDir     string        `yaml:"workdir"`     // empty, dedicated directory for every call
	VersionPin  string        `yaml:"version_pin"` // e.g. "2.1.306"; empty skips the check
	Grace       time.Duration `yaml:"grace"`       // SIGINT → SIGKILL delay, default 15s
	MCPConfig   string        `yaml:"mcp_config"`  // used by agent tasks that need AOE tools
	EmptyMCPCfg string        `yaml:"-"`           // set by New: a config with no servers
}

// Spec describes one call.
type Spec struct {
	TaskType      string
	PromptVersion string
	Model         string          // a --model alias or full name
	SystemPrompt  string          // replaces the default system prompt
	Prompt        string          // sent on stdin
	Schema        json.RawMessage // JSON Schema; when set, structured_output is required
	Tools         []string        // built-in tools; nil or empty disables all
	AllowedTools  []string        // pre-approved tools (permission rules); everything else is denied
	UseMCP        bool            // load Config.MCPConfig (strictly)
	MaxTurns      int
	Timeout       time.Duration
}

// Result is the parsed outcome of a call.
type Result struct {
	Text       string
	Structured json.RawMessage
	SessionID  string
	NumTurns   int
	CostUSD    float64 // the CLI's estimate; not a bill under a subscription
	Usage      json.RawMessage
	Subtype    string
	IsError    bool
	ExitCode   int
	Duration   time.Duration
	Stderr     string
}

// Status classifies a result for the llm_call ledger.
func (r Result) Status(err error) string {
	switch {
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrInvalidOutput):
		return "invalid_output"
	case err != nil || r.IsError:
		return "error"
	default:
		return "ok"
	}
}

var (
	// ErrTimeout means the call exceeded Spec.Timeout.
	ErrTimeout = errors.New("claude: timeout")
	// ErrInvalidOutput means the call finished without the required structured output.
	ErrInvalidOutput = errors.New("claude: missing or invalid structured output")
	// ErrRateLimited means the plan's rate limit was hit; retry later.
	ErrRateLimited = errors.New("claude: rate limited")
)

// Runner executes specs.
type Runner struct {
	cfg Config
}

// New validates the environment and prepares the working directory.
func New(cfg Config) (*Runner, error) {
	if cfg.Binary == "" {
		cfg.Binary = "claude"
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 15 * time.Second
	}
	if cfg.WorkDir == "" {
		return nil, errors.New("claude: workdir is required (an empty, dedicated directory)")
	}
	if err := CheckEnv(os.Environ()); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return nil, err
	}
	cfg.EmptyMCPCfg = cfg.WorkDir + "/.aoe-empty-mcp.json"
	if err := os.WriteFile(cfg.EmptyMCPCfg, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg}, nil
}

// CheckEnv fails if any variable that would override subscription auth is set.
func CheckEnv(environ []string) error {
	var found []string
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		for _, f := range ForbiddenEnv {
			if name == f && value != "" {
				found = append(found, name)
			}
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("claude: %s set in the environment; it would override the subscription token and bill the API. Unset it", strings.Join(found, ", "))
	}
	return nil
}

// childEnv strips forbidden variables defensively and disables WebFetch, which
// returns model-summarised pages that can't be span-checked.
func childEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		skip := false
		for _, f := range ForbiddenEnv {
			if name == f {
				skip = true
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return append(out, "CLAUDE_CODE_DISABLE_WEB_FETCH=1")
}

// Args builds the command line for a spec.
func (r *Runner) Args(s Spec) []string {
	args := []string{
		"-p",
		"--output-format", "json",
		"--permission-mode", "dontAsk",
		"--no-session-persistence",
		"--strict-mcp-config",
		"--tools", strings.Join(s.Tools, ","),
	}
	if s.UseMCP && r.cfg.MCPConfig != "" {
		args = append(args, "--mcp-config", r.cfg.MCPConfig)
	} else {
		args = append(args, "--mcp-config", r.cfg.EmptyMCPCfg)
	}
	if len(s.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(s.AllowedTools, ","))
	}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.SystemPrompt != "" {
		args = append(args, "--system-prompt", s.SystemPrompt)
	}
	if len(s.Schema) > 0 {
		args = append(args, "--json-schema", string(s.Schema))
	}
	if s.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(s.MaxTurns))
	}
	return args
}

// cliResult is the subset of the CLI's JSON result we use.
type cliResult struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	SessionID        string          `json:"session_id"`
	NumTurns         int             `json:"num_turns"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	Usage            json.RawMessage `json:"usage"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

// Run executes a spec. stdout is parsed even on a non-zero exit, because some
// failures are reported as a result object.
func (r *Runner) Run(ctx context.Context, s Spec) (Result, error) {
	if s.Timeout <= 0 {
		s.Timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.cfg.Binary, r.Args(s)...)
	cmd.Dir = r.cfg.WorkDir
	cmd.Env = childEnv(os.Environ())
	cmd.Stdin = strings.NewReader(s.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Graceful stop: SIGINT first so the CLI can finish cleanly, SIGKILL after Grace.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGINT) }
	cmd.WaitDelay = r.cfg.Grace

	start := time.Now()
	runErr := cmd.Run()
	res := Result{Duration: time.Since(start), Stderr: truncate(stderr.String(), 4000)}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("%w after %s", ErrTimeout, s.Timeout)
	}

	var cr cliResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &cr); err != nil {
		if runErr != nil {
			return res, classify(fmt.Errorf("claude exited: %w; stderr: %s", runErr, res.Stderr), res.Stderr)
		}
		return res, fmt.Errorf("claude: unparseable output: %w", err)
	}
	res.Text, res.SessionID, res.NumTurns = cr.Result, cr.SessionID, cr.NumTurns
	res.CostUSD, res.Usage, res.Subtype, res.IsError = cr.TotalCostUSD, cr.Usage, cr.Subtype, cr.IsError
	if len(cr.StructuredOutput) > 0 && string(cr.StructuredOutput) != "null" {
		res.Structured = cr.StructuredOutput
	}

	if cr.IsError || runErr != nil {
		msg := cr.Result
		if msg == "" {
			msg = res.Stderr
		}
		return res, classify(fmt.Errorf("claude error (%s): %s", cr.Subtype, truncate(msg, 1000)), msg)
	}
	if len(s.Schema) > 0 && res.Structured == nil {
		return res, ErrInvalidOutput
	}
	return res, nil
}

// classify maps rate-limit failures to ErrRateLimited so callers can defer.
func classify(err error, text string) error {
	t := strings.ToLower(text)
	if strings.Contains(t, "rate limit") || strings.Contains(t, "rate_limit") || strings.Contains(t, "usage limit") || strings.Contains(t, "429") {
		return fmt.Errorf("%w: %v", ErrRateLimited, err)
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
