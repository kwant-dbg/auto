// Package config loads AOE's runtime configuration (config/aoe.yaml).
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/embed"
	"github.com/kwant-dbg/auto/internal/store"
)

// Config is the runtime configuration. ${VAR} references are expanded from
// the environment before parsing.
type Config struct {
	DatabaseURL string        `yaml:"database_url"`
	BlobDir     string        `yaml:"blob_dir"`
	Claude      claude.Config `yaml:"claude"`
	Worker      Worker        `yaml:"worker"`
	Scheduler   Scheduler     `yaml:"scheduler"`
	Limits      Limits        `yaml:"limits"`
	Models      Models        `yaml:"models"`
	HTTP        HTTP          `yaml:"http"`
	Extraction  Extraction    `yaml:"extraction"`
	Embeddings  embed.Config  `yaml:"embeddings"`

	TOS     []store.TOSEntry   `yaml:"tos"`
	Sources []store.SourceSpec `yaml:"sources"`
}

// Worker tunes the task workers.
type Worker struct {
	Concurrency  int           `yaml:"concurrency"`
	Lease        time.Duration `yaml:"lease"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

// Scheduler tunes the scheduler loop.
type Scheduler struct {
	Tick time.Duration `yaml:"tick"`
}

// Limits are the capacity caps (SCH-7). Claude runs on the subscription, so
// caps are in turns, not dollars.
type Limits struct {
	MaxAgentTurnsPerDay int `yaml:"max_agent_turns_per_day"`
}

// Models maps tiers to Claude Code --model aliases.
type Models struct {
	Fast string `yaml:"fast"`
	Mid  string `yaml:"mid"`
	Top  string `yaml:"top"`
}

// Extraction tunes observation extraction (docs/02 §5).
type Extraction struct {
	Enabled          *bool `yaml:"enabled"` // default true
	MaxQueuedTasks   int   `yaml:"max_queued_tasks"`
	MaxDocsPerBatch  int   `yaml:"max_docs_per_batch"`
	MaxBytesPerBatch int   `yaml:"max_bytes_per_batch"`
}

// On reports whether extraction is enabled.
func (e Extraction) On() bool { return e.Enabled == nil || *e.Enabled }

// HTTP configures outbound fetching by connectors.
type HTTP struct {
	UserAgent string        `yaml:"user_agent"`
	Timeout   time.Duration `yaml:"timeout"`
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(raw))), &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if v := os.Getenv("AOE_DATABASE_URL"); v != "" {
		c.DatabaseURL = v
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.BlobDir == "" {
		c.BlobDir = "data/blobs"
	}
	if c.Claude.WorkDir == "" {
		c.Claude.WorkDir = "data/claude-workdir"
	}
	if c.Worker.Concurrency <= 0 {
		c.Worker.Concurrency = 4
	}
	if c.Worker.Lease <= 0 {
		c.Worker.Lease = 10 * time.Minute
	}
	if c.Worker.PollInterval <= 0 {
		c.Worker.PollInterval = 5 * time.Second
	}
	if c.Scheduler.Tick <= 0 {
		c.Scheduler.Tick = time.Minute
	}
	if c.Limits.MaxAgentTurnsPerDay <= 0 {
		c.Limits.MaxAgentTurnsPerDay = 2000
	}
	if c.Models.Fast == "" {
		c.Models.Fast = "haiku"
	}
	if c.Models.Mid == "" {
		c.Models.Mid = "sonnet"
	}
	if c.Models.Top == "" {
		c.Models.Top = "opus"
	}
	if c.HTTP.UserAgent == "" {
		c.HTTP.UserAgent = "aoe-research/0.1"
	}
	if c.HTTP.Timeout <= 0 {
		c.HTTP.Timeout = 60 * time.Second
	}
	if c.Embeddings.Provider != "" && c.Embeddings.Dimensions == 0 {
		c.Embeddings.Dimensions = 768
	}
}

func (c *Config) validate() error {
	if c.DatabaseURL == "" {
		return errors.New("database_url is required (or set AOE_DATABASE_URL)")
	}
	types := map[string]bool{}
	for _, t := range c.TOS {
		if t.SourceType == "" || t.TermsURL == "" || t.RateLimitPerMin <= 0 {
			return fmt.Errorf("tos entry %q: source_type, terms_url and rate_limit_per_min are required", t.SourceType)
		}
		types[t.SourceType] = true
	}
	names := map[string]bool{}
	for _, s := range c.Sources {
		if s.Name == "" || s.Type == "" {
			return errors.New("every source needs a name and a type")
		}
		if names[s.Name] {
			return fmt.Errorf("duplicate source name %q", s.Name)
		}
		names[s.Name] = true
		if s.Enabled && !types[s.Type] {
			return fmt.Errorf("source %q is enabled but there is no tos entry for type %q", s.Name, s.Type)
		}
	}
	return nil
}
