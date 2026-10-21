package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Health is the outcome of a pre-flight check.
type Health struct {
	Version    string
	AuthMethod string
	LoggedIn   bool
	Raw        json.RawMessage
}

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// Version returns the installed Claude Code version.
func (r *Runner) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.cfg.Binary, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("claude --version: %w", err)
	}
	v := versionRe.FindString(string(out))
	if v == "" {
		return "", fmt.Errorf("claude --version: unrecognised output %q", strings.TrimSpace(string(out)))
	}
	return v, nil
}

// Check verifies the version pin and that the CLI is logged in with the
// subscription token rather than an API key (NFR-11).
func (r *Runner) Check(ctx context.Context) (Health, error) {
	var h Health
	v, err := r.Version(ctx)
	if err != nil {
		return h, err
	}
	h.Version = v
	if r.cfg.VersionPin != "" && v != r.cfg.VersionPin {
		return h, fmt.Errorf("claude: version %s installed, %s pinned; upgrade deliberately and re-run the eval suite", v, r.cfg.VersionPin)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.cfg.Binary, "auth", "status", "--json")
	cmd.Env = childEnv(os.Environ())
	cmd.Dir = r.cfg.WorkDir
	out, err := cmd.Output()
	if len(out) == 0 && err != nil {
		return h, fmt.Errorf("claude auth status: %w", err)
	}
	h.Raw = out
	var status map[string]any
	if jerr := json.Unmarshal(out, &status); jerr != nil {
		return h, fmt.Errorf("claude auth status: unparseable output: %w", jerr)
	}
	if m, ok := status["authMethod"].(string); ok {
		h.AuthMethod = m
	}
	if li, ok := status["loggedIn"].(bool); ok {
		h.LoggedIn = li
	} else {
		h.LoggedIn = err == nil && h.AuthMethod != "" && h.AuthMethod != "none"
	}
	if !h.LoggedIn {
		return h, fmt.Errorf("claude: not logged in; run `claude setup-token` and set CLAUDE_CODE_OAUTH_TOKEN")
	}
	if strings.Contains(strings.ToLower(h.AuthMethod), "api") {
		return h, fmt.Errorf("claude: auth method %q is API-key based; the worker must use the subscription token", h.AuthMethod)
	}
	return h, nil
}

// ProbeWebSearch runs one tiny headless call that may only use WebSearch, to
// confirm the tool is available under subscription auth (docs/06 week 1).
func (r *Runner) ProbeWebSearch(ctx context.Context, model string) (bool, Result, error) {
	schema := json.RawMessage(`{"type":"object","properties":{"websearch_worked":{"type":"boolean"},"top_result_url":{"type":"string"}},"required":["websearch_worked"]}`)
	res, err := r.Run(ctx, Spec{
		TaskType:      "probe.websearch",
		PromptVersion: "probe-v1",
		Model:         model,
		Prompt:        "Use the WebSearch tool once to search for \"O*NET task statements\". Report whether the search returned results and the URL of the top result. If the tool is unavailable, set websearch_worked to false.",
		Schema:        schema,
		Tools:         []string{"WebSearch"},
		AllowedTools:  []string{"WebSearch"},
		MaxTurns:      4,
		Timeout:       3 * time.Minute,
	})
	if err != nil {
		return false, res, err
	}
	var out struct {
		WebsearchWorked bool `json:"websearch_worked"`
	}
	if err := json.Unmarshal(res.Structured, &out); err != nil {
		return false, res, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	return out.WebsearchWorked, res, nil
}
