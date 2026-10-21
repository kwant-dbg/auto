// Package prompts embeds AOE's versioned prompt files. Each task type has one
// directory per version holding system.md and, for structured tasks,
// schema.json. Prompt versions are recorded on every output row.
package prompts

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
)

//go:embed */*/*
var files embed.FS

// Prompt is one versioned prompt.
type Prompt struct {
	Task    string
	Version string
	System  string
	Schema  json.RawMessage
}

// ID is "task/version", stored as prompt_version on outputs.
func (p Prompt) ID() string { return p.Task + "/" + p.Version }

// Load reads a prompt version.
func Load(task, version string) (Prompt, error) {
	dir := path.Join(task, version)
	sys, err := files.ReadFile(path.Join(dir, "system.md"))
	if err != nil {
		return Prompt{}, fmt.Errorf("prompt %s: %w", dir, err)
	}
	p := Prompt{Task: task, Version: version, System: string(sys)}
	if schema, err := files.ReadFile(path.Join(dir, "schema.json")); err == nil {
		if !json.Valid(schema) {
			return Prompt{}, fmt.Errorf("prompt %s: schema.json is not valid JSON", dir)
		}
		p.Schema = schema
	}
	return p, nil
}
