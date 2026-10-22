# Autonomous Opportunity Engine (AOE): Design

Pre-implementation strategy and architecture for a personal system that finds, evaluates, and validates commercially viable software opportunities for a solo technical founder.

**Status:** design review complete and founder decisions recorded (Go; Claude Enterprise subscription via headless Claude Code; Claude-only Bull/Bear/Judge debate; marketplaces as one evidence source among several, with general-purpose tools in scope; week-8 stop rule accepted). Phase 1 build in progress: weeks 1–5 are done (see **Running it** below).

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

### What's built (Phase 1, weeks 1–5)

| Piece | Where | Status |
|---|---|---|
| Full Postgres schema (PRD §8), labour views, extraction tracking, metrics; embedded migrations | `migrations/` | ✅ tested |
| Content-addressed snapshot store, versioned raw documents, prompt-injection tripwire | `internal/store` | ✅ tested |
| Work queue: dedupe keys, `SKIP LOCKED` leases, heartbeats, retries, dead letters, deferral, kill switch | `internal/queue`, `internal/worker` | ✅ tested |
| Headless Claude runner + gateway: schema output, tool allowlists, subscription-auth guard, daily turn cap, rate-limit deferral, call ledger | `internal/claude`, `internal/llm` | ✅ tested; **live**: auth check, WebSearch probe and MCP tools probe all pass |
| Federal service-contract awards (USAspending) | `internal/connectors/usaspending` | ✅ tested; **live**: 327 awards for one day |
| App marketplaces: one connector, Atlassian + WordPress adapters; listings and low-star reviews as text, installs/ratings/prices as daily metrics | `internal/connectors/marketplace` | ✅ tested; **live** against both APIs |
| Practitioner discussion in the founder's domains: Hacker News stories and comments (daily, exact-match tool-name queries) and the most-upvoted open GitHub feature requests in infra projects (weekly) | `internal/connectors/community` | ✅ tested; **live**: HN 391 documents on the first run; GitHub search is blocked in this cloud session and runs where the worker has normal GitHub access |
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
| Weekly digest in the docs/08 format; opportunity cards in one template for AOE and B0 (blind mode hides every source marker); quotes re-span-checked at render time; unchanged cards not resurfaced | `internal/report` | ✅ tested |
| B0 baseline: one web-search session per week with the founder profile, cards in the same fields | `internal/baseline` | ✅ tested; **live** |
| Pairwise ranking of eligible opportunities, both orders, Bradley–Terry | `internal/rank` | ✅ tested |
| Founder feedback and decisions: `rate`, `pick`, `grade`, `decide` (docs/05 transition checks, wake conditions), `answer`, `review-time`; `metrics` for A1–A10 and the stop rule | `internal/feedback`, `internal/lifecycle`, `cmd/aoe` | ✅ tested |

**Live results, week 4.** Research killed both Jira opportunities that reached it on their cheapest check. *Bulk-cloning Jira projects* (opportunity 10) has a verified 4.5★, 11.6k-install, Enterprise Certified incumbent that Atlassian's own help article recommends. *Jira issue templates* (opportunity 26) qualified once research verified its buyer, then failed M4 on two 4.8–4.9★ Cloud incumbents. A post-mortem assessment of 26 gave P50 MRR@12m of $297 (P(≥$1k) 10%, weakest link: price), and the debate upheld the kill 5–1, with the 2029 Data Center end-of-life migration left as the one open thread.

**Live results, week 5: the first full weekly cycle** (fresh database, all fixes in play). Collection snapshotted 2,586 documents: 595 Confluence and 583 Jira listings and reviews, 656 WordPress, and 752 federal service-contract awards (after one back-off retry). Research snapshotted 152 more pages it cited (141 web, 11 HN). Extraction produced 3,003 observations, with 0.3% of quotes rejected. After consolidation, 127 problems have ≥ 2 independent origins. Triage used its daily cap of 40 and research its 30: 3 qualified, 26 candidates and 11 killed (7 `out_of_scope`, 2 `incumbent_strong`, 1 `infeasible`, 1 duplicate). The three qualified opportunities were assessed, and the scheduler built the W43 digest. It has 3 cards, 2 blind pairs against B0 and 3 spot-check findings, and all 38 quotes passed the render-time span check.

The cycle found four bugs, all fixed with regression tests:

- A connection-pool deadlock when four sources collected at once.
- Research scheduling stopped as soon as any opportunity was killed.
- The first digest's blind pairs were recognisable by format alone: B0's descriptive source names, dates, AOE's "more origins" lines, and AOE's unassessed cards.
- Card headers named one observation's job instead of the problem's.

It wasn't unattended: deploying those fixes took restarts and a digest rebuild, so the A9 streak starts next week.

**What the first digest says about AOE vs B0.** B0's cards sit squarely in the founder's SRE/infra domain: Airflow 2 end-of-life migrations (P50 MRR $1.2k) and EKS upgrade-as-a-service (P50 $1.5k). AOE's best card is Jira portfolio and capacity planning (P50 $296, P(≥$1k) 0.13). Its two federal cards, staff training ($74) and expert witnesses ($21), are commodity purchasing categories that passed M1/M2 on spend alone. Expect B0 to win this week's pairs. The likely cause is source selection rather than the pipeline. AOE's enabled sources (Jira, Confluence and WordPress marketplaces, federal contracts) don't overlap the founder's domains, while B0 aims its web search with the profile.

**First review.** The founder rated all three cards not useful, with novelty 1, in 10 minutes. In response, HN and GitHub now feed discovery in the founder's domains, and a card needs P(MRR ≥ $500 at 12 months) ≥ 0.2. That bar alone drops both federal cards (0.04 and 0.00) but keeps the Jira card (0.34).

**Next (week 6):** the second weekly cycle with the new sources, the blind pair picks and finding grades from W43, and a triage rule for procurement-only problems that have spend but no software-shaped obstacle.

### Useful commands

```sh
aoe digest --preview               # this week's digest without saving it (the scheduler builds the real one)
aoe digest --show latest           # the last stored digest
aoe digest --show latest --html --out review.html   # the same digest as the review page (Artifact content; answers save in the page)
aoe card 26                        # one opportunity card
aoe rate 26 useful --novelty 4     # then: aoe pick <pair> A|B, aoe grade <finding> supported, aoe review-time 45
aoe decide 26 park --reason not_now --wake date:2027-01-15
aoe metrics                        # acceptance criteria A1–A10 and the week-8 stop rule
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
