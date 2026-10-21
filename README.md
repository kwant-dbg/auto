# Autonomous Opportunity Engine (AOE): Design

Pre-implementation strategy and architecture for a personal system that finds, evaluates, and validates commercially viable software opportunities for a solo technical founder.

**Status:** design review complete and founder decisions recorded (Go; Claude Enterprise subscription via headless Claude Code; Claude-only Bull/Bear/Judge debate; marketplaces as one evidence source among several, with general-purpose tools in scope; week-8 stop rule accepted). Phase 1 build in progress: week 1 is done (see **Running it** below).

## The core thesis

> With free inference, discovery stops being the bottleneck. Validation and distribution, which both need human contact, take its place.
> AOE should be a **decision-and-validation pipeline with an evidence front end**. An idea factory would only create more work for the scarce resource, the founder's attention.

AOE succeeds if the founder spends their few weekly hours on **opportunities with more evidence, fewer fatal flaws, and named reachable buyers**, and if it beats a $5 one-shot research prompt in blind comparison. If it can't do that after 8 weeks of operation, the plan says to stop building the engine.

## Reading order

| Doc | Contents |
|---|---|
| [`docs/01-critical-assessment.md`](docs/01-critical-assessment.md) | Critique of v1.0: failure modes, questionable assumptions, investor objections, missing capabilities, **the 10 most important improvements** |
| [`docs/02-discovery-methodology.md`](docs/02-discovery-methodology.md) | Ranked discovery lenses (O*NET × BLS wage pools, marketplaces, procurement, paid templates, pay-transparent jobs, change events), creative techniques, dedupe, breadth vs. depth |
| [`docs/03-commercial-evaluation.md`](docs/03-commercial-evaluation.md) | Gates → ranged factors → Monte Carlo EV → value-of-experiment ranking; evidence tiers; what AI can and can't estimate; First-10 plan; defensibility |
| [`docs/04-architecture.md`](docs/04-architecture.md) | 10 agents → 4 LLM roles + code; components; memory model; verification; loop prevention; Temporal vs. Postgres queue; stack; security |
| [`docs/05-lifecycle.md`](docs/05-lifecycle.md) | Revised stages with gates, budgets, kill-first ordering, wake conditions, validation catalogue |
| [`docs/06-mvp-and-roadmap.md`](docs/06-mvp-and-roadmap.md) | Phase 0 Wizard-of-Oz, Phase 1 MVP build sequence and acceptance criteria, later phases, engine stop rule |
| [`docs/07-evaluation-framework.md`](docs/07-evaluation-framework.md) | Six evaluation layers, baselines, backtesting (with leakage/hindsight/survivorship controls), calibration, improvement loops |
| [`docs/08-operations.md`](docs/08-operations.md) | Cadence, the founder's weekly hour, digest & card format, attention and resource limits, governance |
| [`docs/09-open-questions-answered.md`](docs/09-open-questions-answered.md) | Short answers to v1.0's 18 open questions |
| [**`docs/PRD.md`**](docs/PRD.md) | **Revised, implementation-ready PRD v2.0**: requirements with IDs, data model, task catalogue, NFRs, acceptance criteria, founder decisions |

## The 10 most important changes from v1.0

1. **Reframe** from discovery engine to validation pipeline. Measure validation-ready opportunities and founder hit rate, not idea volume.
2. **Make distribution concrete** with the prospect-list test: ≥ 50 named, reachable organisations, plus a channel the founder can personally run.
3. **Replace the additive 100-point score** with gates + ranged factors + multiplicative EV + pairwise ranking. A zero on distribution can no longer hide behind a high demand score.
4. **Strict evidence tiers** with mechanical verbatim-citation checks. Model inference isn't evidence.
5. **Question-driven research.** Tasks answer named critical assumptions and stop when desk research saturates.
6. **Shift discovery to structured, spend-revealing, under-mined sources.** Complaint mining becomes corroboration only.
7. **Founder-fit and founder gates**: employment IP, regulated domains, solo-operability.
8. **Simplify the architecture**: about 4 LLM roles, deterministic orchestration, capability separation for prompt-injection safety.
9. **Move customer contact earlier.** Evaluate service-first and marketplace paths alongside SaaS.
10. **Evaluate against baselines**, calibrate on pre-registered predictions, and pre-commit a stop rule for the engine itself.

## Running it

### What's built (Phase 1, week 1)

| Piece | Where | Status |
|---|---|---|
| Full Postgres schema (PRD §8) + labour views, embedded migrations | `migrations/` | ✅ tested |
| Content-addressed snapshot store, versioned raw documents | `internal/store` | ✅ tested |
| Work queue: dedupe keys, `SKIP LOCKED` leases, heartbeats, retries, dead letters, kill switch | `internal/queue`, `internal/worker` | ✅ tested |
| Headless Claude runner (`claude -p`): schema output, tool allowlists, subscription-auth guard, timeouts, rate-limit detection | `internal/claude` | ✅ tested with a fake CLI; real `claude auth status` check verified |
| ToS register gating, per-source cadence and back-off, per-source run lock | `internal/store`, `internal/collect` | ✅ tested |
| Federal service-contract awards connector (USAspending) | `internal/connectors/usaspending` | ✅ tested; live run: 327 awards for one day, no duplicates on re-run |
| O*NET + BLS OEWS + AI-usage import → ranked task-spend seeds | `internal/labour` | ✅ tested with fixtures |
| `aoe` CLI, `worker`, `scheduler`; Dockerfile + compose | `cmd/`, root | ✅ smoke-tested locally; Docker build not run here |

**Next (week 2):** extractor + span verifier, local embedding server, `aoe-mcp` tool server, generic app-marketplace connector, HN/GitHub search.

### Quick start (Docker)

```sh
claude setup-token                          # once; copy the token
cat > .env <<'ENV'
CLAUDE_CODE_OAUTH_TOKEN=<token>
POSTGRES_PASSWORD=<choose one>
ENV
docker compose up -d --build                # postgres, worker, scheduler
docker compose run --rm worker aoe claude check --probe-websearch
docker compose run --rm worker aoe sources
```

Do **not** put `ANTHROPIC_API_KEY` in the environment. It would override the subscription token and bill the API, so the worker refuses to start with it set.

### Labour-market seeds (discovery lens 1)

Download the data, then import it:

- O*NET text database (`db_<release>_text.zip`) from the O*NET Resource Center. Licence: CC BY 4.0, attribution required.
- BLS OEWS national industry-specific file (`nat3d_M<year>_dl.xlsx` or `nat4d`). Public domain.
- Optional: a task-level AI-usage table such as the Anthropic Economic Index (CC BY 4.0). Column names vary by release.

```sh
aoe import onet --release 29.3 db_29_3_text.zip
aoe import oews --year 2024 nat4d_M2024_dl.xlsx
aoe import ai-usage --release 2026-01 --task-col task_name --value-col pct onet_task_mappings.csv
aoe seeds --limit 50              # or --naics 6212 for one industry, --cross-industry for all
```

### Development

```sh
make db        # disposable pgvector container on :55432
make test      # all tests, race detector on (DB tests skip without AOE_TEST_DATABASE_URL)
make vet build
```
