package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// WriteMCPConfig writes the MCP config that gives agent tasks AOE's tools
// (the aoe-mcp server over stdio) and points the runner at it. env is passed
// to the server; the file is private because it can contain the database URL.
func (r *Runner) WriteMCPConfig(command string, env map[string]string) (string, error) {
	path := filepath.Join(r.cfg.WorkDir, ".aoe-mcp.json")
	cfg := map[string]any{"mcpServers": map[string]any{
		"aoe": map[string]any{"type": "stdio", "command": command, "args": []string{}, "env": env},
	}}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	r.cfg.MCPConfig = path
	return path, nil
}

// AOETools are the MCP tool names agents may be allowed to call.
var AOETools = []string{
	"mcp__aoe__fetch_and_snapshot",
	"mcp__aoe__kb_search",
	"mcp__aoe__search_hn",
	"mcp__aoe__search_github_issues",
}

// ProbeTools runs one headless agent call that may only use AOE's kb_search
// tool, to confirm the MCP server starts and its tools are reachable.
func (r *Runner) ProbeTools(ctx context.Context, model, query string) (bool, int, Result, error) {
	if r.cfg.MCPConfig == "" {
		return false, 0, Result{}, fmt.Errorf("no MCP config: aoe-mcp was not found")
	}
	schema := json.RawMessage(`{"type":"object","properties":{"tool_worked":{"type":"boolean"},"hits":{"type":"integer"}},"required":["tool_worked","hits"]}`)
	res, err := r.Run(ctx, Spec{
		TaskType:      "probe.tools",
		PromptVersion: "probe-v1",
		Model:         model,
		Prompt:        fmt.Sprintf("Call the kb_search tool once with the query %q. Report whether the call succeeded and how many hits it returned. If the tool is unavailable, set tool_worked to false and hits to 0.", query),
		Schema:        schema,
		UseMCP:        true,
		AllowedTools:  []string{"mcp__aoe__kb_search"},
		MaxTurns:      4,
		Timeout:       3 * time.Minute,
	})
	if err != nil {
		return false, 0, res, err
	}
	var out struct {
		ToolWorked bool `json:"tool_worked"`
		Hits       int  `json:"hits"`
	}
	if err := json.Unmarshal(res.Structured, &out); err != nil {
		return false, 0, res, fmt.Errorf("%w: %v", ErrInvalidOutput, err)
	}
	return out.ToolWorked, out.Hits, res, nil
}
