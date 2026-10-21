# Autonomous Opportunity Engine (AOE): Design

Pre-implementation strategy and architecture for a personal system that finds, evaluates, and validates commercially viable software opportunities for a solo technical founder.

**Status:** design review complete and founder decisions recorded (Go; Claude Enterprise subscription via headless Claude Code; Claude-only Bull/Bear/Judge debate; marketplaces as one evidence source among several, with general-purpose tools in scope; week-8 stop rule accepted). Phase 1 build in progress: weeks 1–4 are done (see **Running it** below).

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

### What's built (Phase 1, weeks 1–4)

| Piece | Where | Status |
|---|---|---|
| Full Postgres schema (PRD §8), labour views, extraction tracking, metrics; embedded migrations | `migrations/` | ✅ tested |
| Content-addressed snapshot store, versioned raw documents, prompt-injection tripwire | `internal/store` | ✅ tested |
| Work queue: dedupe keys, `SKIP LOCKED` leases, heartbeats, retries, dead letters, deferral, kill switch | `internal/queue`, `internal/worker` | ✅ tested |
| Headless Claude runner + gateway: schema output, tool allowlists, subscription-auth guard, daily turn cap, rate-limit deferral, call ledger | `internal/claude`, `internal/llm` | ✅ tested; **live**: auth check, WebSearch probe and MCP tools probe all pass |
| Federal service-contract awards (USAspending) | `internal/connectors/usaspending` | ✅ tested; **live**: 327 awards for one day |
| App marketplaces: one connector, Atlassian + WordPress adapters; listings and low-star reviews as text, installs/ratings/prices as daily metrics | `internal/connectors/marketplace` | ✅ tested; **live** against both APIs |
| O*NET + BLS OEWS + AI-usage import → ranked task-spend seeds | `internal/labour` | ✅ tested with fixtures |
| Extraction: verbatim quotes located by code, tier by rule, origin keys, rejected quotes logged; batch scheduler with backpressure | `internal/extract`, `internal/evidence`, `internal/jobs` | ✅ tested; **live**: 29 documents → 27 observations, 0 rejected quotes |
| Embeddings (Ollama or TEI) + sweeper | `internal/embed` | ✅ tested against fake servers; Ollama itself not run here |
| `aoe-mcp` tools: `fetch_and_snapshot`, `kb_search`, `search_hn`, `search_github_issues` | `internal/tools`, `cmd/aoe-mcp` | ✅ tested (incl. an in-memory MCP round trip); **live**: fetch, HN; GitHub search untested live (blocked in the build sandbox) |
| Problems: candidate search against every member (pgvector, or pg_trgm without embeddings), auto attach/create, batched model adjudication with hierarchy; every decision logged | `internal/problems` | ✅ tested |
| Consolidation: merges duplicate problems (one product would serve both), closes duplicate opportunities; triage waits until it has caught up | `internal/problems` | ✅ tested; **live**: 248 merges over 1,810 observations |
| Triage → opportunities: founder profile, code-enforced gate rules, lifecycle decision with reason codes, kill questions, re-triage when evidence doubles | `internal/triage`, `internal/founder` | ✅ tested; **live**: 40 problems triaged (daily cap) |
| CLI, worker, scheduler; Dockerfile + compose (Postgres, worker, scheduler, Ollama, Metabase) | `cmd/`, root | ✅ smoke-tested locally; Docker build not run here |

| Research agent: answers questions with WebSearch + `aoe-mcp`, cites only snapshotted documents; findings verified (span check + entailment); gates change only on supported findings; budgets, saturation, lifecycle | `internal/research`, `internal/verify`, `internal/lifecycle` | ✅ tested; **live** (see below) |
| Assessor: ranged factors with citations, wide priors for unsupported factors, seeded Monte Carlo (P10/P50/P90 MRR@12m), weakest link | `internal/assess` | ✅ tested |
| Debate: Bull and Bear in parallel independent sessions, two cited rounds, unsupported arguments struck, fresh Judge | `internal/debate` | ✅ tested |

**Live results, week 4.** Research killed both Jira opportunities that reached it on their cheapest check. *Bulk-cloning Jira projects* (opportunity 10) has a verified 4.5★, 11.6k-install, Enterprise Certified incumbent that Atlassian's own help article recommends. *Jira issue templates* (opportunity 26) qualified once research verified its buyer, then failed M4 on two 4.8–4.9★ Cloud incumbents. A post-mortem assessment of 26 gave P50 MRR@12m of $297 (P(≥$1k) 10%, weakest link: price), and the debate upheld the kill 5–1, with the 2029 Data Center end-of-life migration left as the one open thread.

**Next (week 5):** the weekly digest and opportunity cards, CLI feedback (ratings, pairwise picks, finding grades), the one-shot-prompt baseline for the blind comparison, and the first full weekly cycle.

### Useful commands

```sh
aoe stats                          # documents, extraction progress, rejection rate, observations by tier, Claude usage today
aoe problems --min-origins 2       # problems backed by independent evidence
aoe opportunities                  # triaged opportunities with gate verdicts and open questions
aoe opportunity <id>               # one opportunity: summary, gates with citations, questions
aoe observations --tier 1          # strongest evidence first
aoe kb "month-end reconciliation"  # search collected evidence
aoe extract --dry-run --limit 3    # see the exact prompt the extractor will send
aoe fetch <url> ; aoe hn <query>   # the agents' fetch and Hacker News tools, by hand
aoe claude check --probe-tools --probe-websearch
```

### Quick start (Docker)

```sh
claude setup-token                          # once; copy the token
cat > .env <<'ENV'
CLAUDE_CODE_OAUTH_TOKEN=<token>
POSTGRES_PASSWORD=<choose one>
ENV
docker compose up -d --build                # postgres, worker, scheduler, ollama (+ model pull)
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
