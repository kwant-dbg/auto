package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kwant-dbg/auto/internal/assess"
	"github.com/kwant-dbg/auto/internal/baseline"
	"github.com/kwant-dbg/auto/internal/claude"
	"github.com/kwant-dbg/auto/internal/config"
	"github.com/kwant-dbg/auto/internal/debate"
	"github.com/kwant-dbg/auto/internal/embed"
	"github.com/kwant-dbg/auto/internal/extract"
	"github.com/kwant-dbg/auto/internal/founder"
	"github.com/kwant-dbg/auto/internal/jobs"
	"github.com/kwant-dbg/auto/internal/llm"
	"github.com/kwant-dbg/auto/internal/problems"
	"github.com/kwant-dbg/auto/internal/queue"
	"github.com/kwant-dbg/auto/internal/rank"
	"github.com/kwant-dbg/auto/internal/report"
	"github.com/kwant-dbg/auto/internal/research"
	"github.com/kwant-dbg/auto/internal/store"
	"github.com/kwant-dbg/auto/internal/triage"
	"github.com/kwant-dbg/auto/internal/verify"
	"github.com/kwant-dbg/auto/internal/worker"
)

// NewLLM checks Claude Code (version pin, subscription auth), writes the MCP
// config for AOE's tools and returns the gateway all model calls go through.
func NewLLM(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger) (*llm.Client, *claude.Runner, error) {
	runner, err := claude.New(cfg.Claude)
	if err != nil {
		return nil, nil, err
	}
	health, err := runner.Check(ctx)
	if err != nil {
		return nil, nil, err
	}
	if bin, err := mcpBinary(cfg); err != nil {
		log.Warn("aoe-mcp not found; agent tasks will have no AOE tools", "err", err)
	} else {
		cfgPath, _ := filepath.Abs(ConfigPath())
		env := map[string]string{"AOE_CONFIG": cfgPath, "AOE_DATABASE_URL": cfg.DatabaseURL, "AOE_BLOB_DIR": cfg.BlobDir}
		if t := os.Getenv("GITHUB_TOKEN"); t != "" {
			env["GITHUB_TOKEN"] = t
		}
		if _, err := runner.WriteMCPConfig(bin, env); err != nil {
			return nil, nil, err
		}
	}
	return &llm.Client{
		Runner: runner, Store: st, MaxTurnsPerDay: cfg.Limits.MaxAgentTurnsPerDay, ClaudeCodeVersion: health.Version,
	}, runner, nil
}

func mcpBinary(cfg *config.Config) (string, error) {
	name := cfg.Claude.MCPBinary
	if name == "" {
		name = "aoe-mcp"
	}
	if p, err := exec.LookPath(name); err == nil {
		return filepath.Abs(p)
	}
	// Next to the running binary (e.g. bin/worker and bin/aoe-mcp).
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), filepath.Base(name))
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return exec.LookPath(name)
}

// RegisterHandlers wires every task type the worker can run. Collection
// always works; model-backed tasks need a healthy Claude Code, and embedding
// needs a configured embedding server.
func RegisterHandlers(ctx context.Context, w *worker.Worker, cfg *config.Config, st *store.Store, log *slog.Logger) {
	w.Handle(jobs.TaskCollect, jobs.CollectHandler(st, CollectOptions(cfg)))
	w.Handle(report.DigestTaskType, DigestHandler(cfg, st))

	client, runner, err := NewLLM(ctx, cfg, st, log)
	if err != nil {
		log.Error("Claude Code check failed; model-backed tasks are disabled until it passes", "err", err)
	} else {
		ex := &extract.Extractor{Store: st, LLM: client, Model: cfg.Models.Fast}
		w.Handle(extract.TaskType, ex.Handle)
		cl := Clusterer(cfg, st, client)
		w.Handle(problems.TaskType, cl.Handle)
		w.Handle(problems.ConsolidateTaskType, cl.ConsolidateHandle)
		tr, err := Triager(cfg, st, client)
		if err != nil {
			log.Error("triage and assessment disabled: founder profile", "err", err)
		} else {
			w.Handle(triage.TaskType, tr.Handle)
			w.Handle(assess.TaskType, Assessor(cfg, st, client, tr.Founder).Handle)
		}
		w.Handle(research.TaskType, Researcher(cfg, st, client).Handle)
		if tr != nil {
			w.Handle(baseline.TaskType, Baseline(cfg, st, client, tr.Founder).Handle)
			w.Handle(rank.TaskType, (&rank.Ranker{Store: st, LLM: client, Model: cfg.Models.Mid, Profile: tr.Founder.Summary()}).Handle)
		}
		w.Handle(debate.TaskType, Debater(cfg, st, client).Handle)
		go recheck(ctx, runner, log)
	}

	if cfg.Embeddings.Enabled() {
		ec, err := embed.New(cfg.Embeddings)
		if err != nil {
			log.Error("embeddings disabled", "err", err)
		} else {
			sw := &embed.Sweeper{Store: st, Client: ec}
			w.Handle(embed.TaskType, sw.Handle)
		}
	}
}

// recheck repeats the auth and version check hourly (NFR-11), so an expired
// token shows up in the logs promptly.
func recheck(ctx context.Context, r *claude.Runner, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.Check(ctx); err != nil {
				log.Error("Claude Code health check failed", "err", err)
			}
		}
	}
}

// Clusterer builds the problem clusterer; client may be nil (no adjudication).
func Clusterer(cfg *config.Config, st *store.Store, client *llm.Client) *problems.Clusterer {
	c := &problems.Clusterer{Store: st, Model: cfg.Models.Mid, Cfg: cfg.Clustering}
	if client != nil {
		c.LLM = client
	}
	return c
}

// Triager builds the triager with the founder profile.
func Triager(cfg *config.Config, st *store.Store, client *llm.Client) (*triage.Triager, error) {
	prof, err := founder.Load(cfg.FounderProfile)
	if err != nil {
		return nil, err
	}
	return &triage.Triager{Store: st, LLM: client, Model: cfg.Models.Mid, Founder: prof}, nil
}

// Verifier builds the claim verifier (mid-tier model for entailment).
func Verifier(cfg *config.Config, st *store.Store, client *llm.Client) *verify.Verifier {
	return &verify.Verifier{Store: st, LLM: client, Model: cfg.Models.Mid}
}

// Baseline builds the B0 runner (top-tier model, web search only).
func Baseline(cfg *config.Config, st *store.Store, client *llm.Client, prof *founder.Profile) *baseline.Runner {
	return &baseline.Runner{Store: st, LLM: client, Model: cfg.Models.Top, Profile: prof.Summary(), Cards: cfg.Baseline.Cards,
		MaxTurns: cfg.Baseline.MaxTurns, SkipF1: !prof.Founder.EmploymentConstraints.ConflictCheckRequired}
}

// DigestHandler builds the week's digest. It waits up to BaselineWait past
// the due time for the week's B0 run, so the blind pairs have cards.
func DigestHandler(cfg *config.Config, st *store.Store) worker.Handler {
	b := &report.Builder{Store: st, Opt: cfg.Digest}
	return func(ctx context.Context, t *queue.Task) (any, error) {
		var in report.DigestInputs
		if err := json.Unmarshal(t.Inputs, &in); err != nil {
			return nil, queue.Permanent(err)
		}
		now := time.Now()
		pending, err := baseline.Pending(ctx, st, in.Week)
		if err != nil {
			return nil, err
		}
		var assessing bool
		if err := st.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task WHERE type = $1 AND state IN ('queued', 'running'))`, assess.TaskType).Scan(&assessing); err != nil {
			return nil, err
		}
		if (pending || assessing) && now.Before(in.Due.Add(b.Opt.BaselineWait)) {
			reason := "waiting for this week's B0 baseline run"
			if !pending {
				reason = "waiting for assessments in flight, so cards carry them"
			}
			return nil, &worker.DeferError{Until: now.Add(5 * time.Minute), Reason: reason}
		}
		return b.Build(ctx, now, "system", true)
	}
}

// Researcher builds the research agent (top-tier model).
func Researcher(cfg *config.Config, st *store.Store, client *llm.Client) *research.Researcher {
	return &research.Researcher{Store: st, LLM: client, Verifier: Verifier(cfg, st, client), Model: cfg.Models.Top, MaxTurns: cfg.Research.MaxTurns}
}

// Assessor builds the assessor (top-tier model).
func Assessor(cfg *config.Config, st *store.Store, client *llm.Client, prof *founder.Profile) *assess.Assessor {
	return &assess.Assessor{Store: st, LLM: client, Model: cfg.Models.Top, Founder: prof}
}

// Debater builds the debate (top-tier model for Bull, Bear and Judge).
func Debater(cfg *config.Config, st *store.Store, client *llm.Client) *debate.Debater {
	return &debate.Debater{Store: st, LLM: client, Verifier: Verifier(cfg, st, client), Model: cfg.Models.Top}
}

// ExtractOptions maps config to scheduling options.
func ExtractOptions(cfg *config.Config) jobs.ExtractOptions {
	return jobs.ExtractOptions{
		MaxQueuedTasks:   cfg.Extraction.MaxQueuedTasks,
		MaxDocsPerBatch:  cfg.Extraction.MaxDocsPerBatch,
		MaxBytesPerBatch: cfg.Extraction.MaxBytesPerBatch,
	}
}
