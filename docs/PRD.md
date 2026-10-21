# Autonomous Opportunity Engine (AOE): Product Requirements Document

**Version:** 2.0 (supersedes v1.0)
**Status:** Implementation-ready for Phase 0–1; later phases specified at design level
**Rationale documents:** `01`–`09` in this directory. This PRD states *what* to build. The numbered documents explain *why*.

---

## 1. Purpose

AOE is a personal system for one technical founder. It turns abundant AI inference into a **small number of evidence-backed, distribution-checked, pre-killed opportunities**, each with a ready-to-run validation plan, so that the founder's scarce hours go into talking to and selling to real buyers.

**AOE is a decision-and-validation pipeline with an evidence-gathering front end. It isn't an idea generator.**

## 2. Goals

| ID | Goal | Measure |
|---|---|---|
| G1 | Surface opportunities the founder wouldn't have found, backed by evidence they trust | Usefulness ≥ 40%; novelty rating; citation validity 100% |
| G2 | Beat cheap baselines | Blind pairwise win rate vs. one-shot prompt ≥ 65% |
| G3 | Produce validation-ready opportunities | ≥ 1 per month with ≥ 50-org prospect list + pre-registered plan |
| G4 | Minimise human attention | ≤ 60 min/week review; ≤ 3 cards/week |
| G5 | Learn and calibrate | Brier scores tracked per factor; source-yield reallocation |
| G6 | Ultimately: commercial value per founder-hour | First paying customer → $1k → $5k MRR |

## 3. Non-goals (v2.0)

Autonomous outreach or purchasing; autonomous public publishing or deployment; multi-tenant SaaS; a custom dashboard (Phase 1–3); Temporal/Kubernetes/Kafka/graph DB in Phase 1; large-scale crawling; collection from sources whose terms prohibit it; venture-scale market sizing; agent-count or token-usage targets.

## 4. User and constraints

**Single user:** a founder-engineer (Go, Kubernetes, SRE/platform, Temporal, LLM infra) with limited weekly hours.

**Founder profile** (config, required before Phase 1 operation):

```yaml
founder:
  weekly_review_minutes: 60
  weekly_validation_hours: 4          # time available for customer contact
  domains_with_access:                # credibility / network, 0–3
    sre_platform_engineering: 3
    llm_infrastructure: 3
    workflow_orchestration: 2
  excluded_domains: [regulated_legal_advice, medical, securities]
  employment_constraints:
    conflict_check_required: false     # F1 gate disabled by founder decision
  solo_operability:
    max_sales_cycle_days: 60
    allow_24x7_sla: false
    allow_pre_sale_security_audit: false
commercial_targets:
  milestones_mrr: [1, 1000, 5000]
  preferred_price_band_usd_month: [150, 600]
  horizon_months: 12
budgets:
  daily_llm_usd_cap: <set>
  daily_paid_api_usd_cap: <set>
  max_concurrent_agent_tasks: 4
attention:
  cards_per_digest: 3
  approvals_per_week: 5
  concurrent_validations: 2
```

## 5. Core concepts

| Concept | Definition |
|---|---|
| **RawDocument** | An immutable snapshot of fetched content with hash, URL, timestamps |
| **Observation** | An atomic claim extracted from a RawDocument, with **verbatim span offsets**, normalised job statement, evidence tier, specificity |
| **Problem** | A cluster of observations describing the same `actor | job | obstacle | consequence`; hierarchical |
| **Opportunity** | A **problem × segment** under evaluation, with gates, factors, paths, questions, state |
| **Question** | A critical assumption for an opportunity, linked to a factor or gate; the unit of research work |
| **Finding** | A research result answering a question; cites observations/snapshots; verified |
| **Experiment** | A human-approved external test with pre-registered predictions and thresholds |
| **Decision** | An append-only state transition or human judgment with reason code and evidence |
| **Evidence tiers** | T0 commitment to us · T1 observed third-party spend · T2 direct statement to us · T3 published offer · T4 specific complaint · T5 engagement · T6 commentary · T7 model inference (not evidence) |

## 6. Functional requirements

Priority: **P0** = Phase 1 must-have · **P1** = Phase 2 · **P2** = Phase 3+.

### 6.1 Source collection

| ID | Requirement | P |
|---|---|---|
| SRC-1 | Each source is a connector module implementing `fetch_incremental(cursor) → [RawDocument], new_cursor` | P0 |
| SRC-2 | A connector can't be enabled without a ToS-register entry (access method, terms reviewed date, allowed use, rate limit) | P0 |
| SRC-3 | Connectors enforce per-source rate limits and record request counts | P0 |
| SRC-4 | RawDocuments are content-addressed; identical content is not stored twice; changed content creates a new version linked to the previous one | P0 |
| SRC-5 | Store `published_at` (if known), `fetched_at`, canonical URL, source type, author/org identifiers (hashed where possible) | P0 |
| SRC-6 | Per-source cadence with automatic back-off after N runs yielding no new verified observations | P0 |
| SRC-7 | Source health record: last success, last new observation, error rate | P0 |
| SRC-8 | Phase 1 connectors: O*NET + BLS OEWS + AI-usage index (static import); public procurement awards API; one app marketplace with permitted access; HN + GitHub search (as research tools) | P0 |
| SRC-9 | Change monitors: tracked-URL diff (pricing pages, changelogs), regulatory feed, shutdown notices | P1 |
| SRC-10 | ATS public job-board connector over a curated company list | P1 |
| SRC-11 | Paid-template / productised-service sampling (manual-rate, terms-respecting) | P1 |

### 6.2 Extraction and verification

| ID | Requirement | P |
|---|---|---|
| EXT-1 | Extractor is an LLM call with **no tools and no network**, constrained to a JSON schema | P0 |
| EXT-2 | Each observation includes `span_start`, `span_end`, `quote`; the verifier rejects any whose quote ≠ snapshot slice (whitespace-normalised) | P0 |
| EXT-3 | Each observation includes a normalised statement: `actor_segment`, `job`, `obstacle`, `consequence` | P0 |
| EXT-4 | Evidence tier is assigned **by rule** from (source type, extraction category), not by the LLM's opinion | P0 |
| EXT-5 | Extract spend hints (amounts, frequencies, headcount, tools) as structured fields when present | P0 |
| EXT-6 | Flag instruction-like content aimed at AI systems (`injection_suspect`) | P0 |
| EXT-7 | Store `model`, `prompt_version`, `input_hash` on every observation | P0 |

### 6.3 Problems and deduplication

| ID | Requirement | P |
|---|---|---|
| PRB-1 | Embed normalised statements (pinned embedding model recorded per vector) | P0 |
| PRB-2 | Attach to existing problem above threshold `τ_high`; adjudicate by LLM in `[τ_low, τ_high)` with verdict ∈ {same, parent, child, sibling, different}; else create candidate | P0 |
| PRB-3 | Problems form a hierarchy (parent_id) | P0 |
| PRB-4 | Compute independent-origin count per problem (author/org/near-dup/canonical-URL clustering) | P0 |
| PRB-5 | Weekly re-clustering with LLM-reviewed merge/split proposals | P1 |
| PRB-6 | Thresholds calibrated against a labelled pair set; report precision/recall | P0 |

### 6.4 Opportunities, gates, questions

| ID | Requirement | P |
|---|---|---|
| OPP-1 | Triage creates an Opportunity from a Problem meeting entry criteria (≥ 2 independent origins or 1 T1 observation) | P0 |
| OPP-2 | Gates F1–F4 and M1–M6 (see `03` §2.1) each hold PASS/FAIL/UNKNOWN with evidence links; F1 is skipped when `conflict_check_required: false` (current setting) | P0 |
| OPP-3 | Each UNKNOWN gate and each factor with evidence ≤ T6 creates a Question | P0 |
| OPP-4 | Kill questions (competitor sweep, prospect-list feasibility, unit-cost sanity, spend evidence) are created first and ordered by P(kill)/cost | P0 |
| OPP-5 | Factors are stored as (low, base, high) + evidence tier + citations; factors with only T6–T7 evidence are clamped to configured wide priors | P0 |
| OPP-6 | Code computes Monte Carlo EV distribution (P10/P50/P90 MRR@12m, P(≥ $1k), P(≥ $5k)) and weakest-link sensitivity | P0 |
| OPP-7 | At least 2 business-model paths evaluated per Dossier-stage opportunity (service-first + one other) | P1 (P0: service-first only) |
| OPP-8 | First-10 plan: prospect list (≥ 50 orgs for M3 PASS), primary + backup channel, founder-executability questions, ranged funnel, kill thresholds | P0 (list ≥ 20 acceptable in Phase 1), P1 full |
| OPP-9 | Parked opportunities require ≥ 1 machine-checkable wake condition; nightly job evaluates them | P0 |
| OPP-10 | Killed opportunities require a reason code from the fixed taxonomy | P0 |

### 6.5 Research

| ID | Requirement | P |
|---|---|---|
| RES-1 | Researcher tasks answer exactly one Question; output = Findings with citations | P0 |
| RES-2 | Tools: `web_search`, `fetch_and_snapshot` (stores a RawDocument before anything can cite it), `kb_search`, `propose_question`; all read-only | P0 |
| RES-3 | Findings are verified: span check (deterministic) + entailment (separate LLM call, overclaiming rubric) → {supported, partial, unsupported} | P0 |
| RES-4 | Only `supported` findings update factors; `partial` may widen ranges only | P0 |
| RES-5 | Debate runs on each Dossier (`04` §4.1): Bull and Bear in independent Claude sessions, 2 cited rounds, uncited arguments struck, fresh Judge rules per contested claim; all blinded to EV/scores; outputs surviving objections, gate updates and kill-questions | P0 |
| RES-6 | Dual independent research for opportunities entering Validating; disagreements become Questions | P1 |
| RES-7 | Prospect-list builder tool (directories, licence registries, associations, marketplace vendor pages) | P1 (P0: via generic research) |

### 6.6 Scheduling and control

| ID | Requirement | P |
|---|---|---|
| SCH-1 | Postgres-backed task queue with leases, priorities, retries (max 2), dead-letter | P0 |
| SCH-2 | Task dedupe key `(type, opportunity_id, question_hash, evidence_version)` | P0 |
| SCH-3 | Earned research budget per opportunity by stage (Candidate 1, Qualified 5, Dossier 20–50 units) | P0 |
| SCH-4 | Proposed follow-up questions: ≤ 3 per task, depth ≤ 2, admitted only if linked to factor/gate and within budget | P0 |
| SCH-5 | Saturation: two consecutive tasks with no material change (no range narrowed > 20%, no gate flipped) → mark desk-research saturated; escalate | P0 |
| SCH-6 | Nightly queue regeneration ordered by value-of-experiment ÷ cost | P0 |
| SCH-7 | Global caps: concurrency, daily LLM $, daily paid-API $, browser sessions; exceeding pauses admission | P0 |
| SCH-8 | Kill switch: pause all admission and actions | P0 |

### 6.7 Ranking, reporting, feedback

| ID | Requirement | P |
|---|---|---|
| RPT-1 | Weekly digest in fixed format (`08` §2), ≤ configured card count; explicitly states "no opportunity met the bar" when applicable | P0 |
| RPT-2 | Pairwise LLM ranking among eligible opportunities, both presentation orders, Bradley-Terry aggregation | P0 |
| RPT-3 | Digest includes 2 blind pairs vs. baseline B0 cards and 3 random findings for spot-check | P0 |
| RPT-4 | CLI: `aoe digest`, `aoe card <id>`, `aoe rate <id> <useful|not> [reason]`, `aoe pick <pairId> <A|B> [reason]`, `aoe grade <findingId> <supported|overclaimed|wrong>`, `aoe decide <oppId> <advance|park|kill> --reason <code> [--wake ...]`, `aoe approve|deny <actionId>`, `aoe budget`, `aoe pause|resume` | P0 |
| RPT-5 | Read-only SQL exploration UI (Datasette or Metabase) | P0 |
| RPT-6 | Monthly report: source yield, reason codes, calibration, consistency, costs | P1 |
| RPT-7 | Case-based retrieval of similar past decisions into triage/ranking prompts | P1 |

### 6.8 Validation (Phase 3)

| ID | Requirement | P |
|---|---|---|
| VAL-1 | Experiment records: hypothesis, linked factors, method, sample, pre-registered probabilistic predictions, success/kill thresholds, budget caps | P2 |
| VAL-2 | System prepares all materials (prospect subset, personalised drafts, interview guide, landing copy) as approval items | P2 |
| VAL-3 | Action executor: deterministic, idempotent, the only component with external-write credentials; executes only approved structured actions | P2 |
| VAL-4 | Result ingestion: transcripts → T2 observations; replies/payments → T0/T1 | P2 |
| VAL-5 | Brier scoring and per-factor optimism shrinkage | P2 |

### 6.9 Building (Phase 4)

| ID | Requirement | P |
|---|---|---|
| BLD-1 | Builder runs in a sandbox: rootless container, no secrets, egress allowlist, CPU/mem/time limits | P2 |
| BLD-2 | Each prototype targets one named riskiest assumption with a benchmark and pass threshold | P2 |
| BLD-3 | Independent agent code review + founder review before any deployment; deployment is an approval item | P2 |

## 7. Task-type catalogue (Phase 1)

| Task type | Kind | Model tier | Tools | Output schema |
|---|---|---|---|---|
| `collect.<source>` | code | none | network (connector only) | RawDocument[] |
| `extract` | LLM fn | fast | none | Observation[] |
| `adjudicate_pair` | LLM fn | mid | none | verdict |
| `triage` | LLM fn + code | mid | none | gates, initial questions |
| `research.<qtype>` (qtype ∈ competitor, prospects, unit_cost, spend, market, channel, technical, defensibility) | agent | top | web_search, fetch_and_snapshot, kb_search, propose_question | Finding[] |
| `verify_entailment` | LLM fn | mid | none | supported/partial/unsupported |
| `assess` | LLM fn + code | top | none | factor ranges, paths, First-10 plan, next experiments |
| `debate_bull`, `debate_bear` | agent | top (independent sessions) | web_search, fetch_and_snapshot, kb_search | Argument[] (cited) |
| `debate_judge` | LLM fn | top (fresh session) | none | Ruling[], Objection[], Question[] |
| `rank_pair` | LLM fn | top | none | choice + deciding evidence |
| `baseline_b0` | LLM fn/agent | top | web_search | Card[] |
| `digest` | code | none | none | markdown |

## 8. Data model

Postgres 16 + pgvector. All tables have `id` (UUID/ULID), `created_at`. LLM-produced rows carry `model`, `prompt_version`, `input_hash`.

| Table | Key columns | Notes |
|---|---|---|
| `source` | type, name, config, cadence, cursor, tos_entry_id, enabled, health fields | |
| `tos_entry` | source_type, access_method, terms_url, reviewed_at, allowed_use, rate_limit, notes | Required for `source.enabled` |
| `raw_document` | source_id, canonical_url, content_hash, blob_path, mime, published_at, fetched_at, prev_version_id, author_hash, org_hint, injection_suspect | Immutable |
| `observation` | raw_document_id, span_start, span_end, quote, category, evidence_tier, actor_segment, job, obstacle, consequence, specificity, spend_hints (jsonb), origin_key, embedding (vector), embedding_model, verified (bool) | `origin_key` drives independence |
| `problem` | parent_id, canonical_statement, segment, industry_naics, embedding, independent_origin_count, status | |
| `problem_observation` | problem_id, observation_id, attach_method (auto/adjudicated), score | |
| `opportunity` | problem_id, segment, state, stage_entered_at, budget_units_total, budget_units_used, saturated (bool), current_assessment_id | State derived from latest `decision` |
| `gate_status` | opportunity_id, gate_code, status (PASS/FAIL/UNKNOWN), evidence_ids, updated_at | |
| `factor_estimate` | opportunity_id, factor_code, low, base, high, unit, evidence_tier_max, citation_ids, assessment_id | Versioned by assessment |
| `assessment` | opportunity_id, ev_p10, ev_p50, ev_p90, p_ge_1k, p_ge_5k, weakest_factor, paths (jsonb), first10_plan (jsonb), red_team_summary | |
| `prospect` | opportunity_id, org_name, segment_fit_evidence_id, contact_path, source_url, status | Organisations only |
| `question` | opportunity_id, text, linked_factor/gate, kind (kill/standard), status (open/answered/saturated/needs_human), voe_estimate, cost_estimate, parent_question_id, depth, origin (triage/agent/human) | |
| `finding` | question_id, claim, citation_observation_ids, citation_spans, verdict (supported/partial/unsupported), retracted (bool), retraction_reason | |
| `objection` | opportunity_id, text, evidence_ids, severity, later_outcome | `later_outcome` lets red-team quality be scored |
| `experiment` | opportunity_id, hypothesis, factor_codes, method, sample, predictions (jsonb), success_threshold, kill_threshold, budget, results, brier, conclusion | Phase 3 |
| `action` | experiment_id, type, payload (jsonb), idempotency_key, status (proposed/approved/denied/executed/failed), approved_by, executed_at | Phase 3 |
| `decision` | subject_type, subject_id, decision_type, from_state, to_state, actor (human/system), reason_code, notes, evidence_ids, factor_snapshot (jsonb), wake_conditions (jsonb) | Append-only |
| `feedback` | subject_type, subject_id, kind (rating/pairwise/grade), value, reason_code | |
| `prediction` | experiment_id, factor_code, statement, probability, outcome, scored_at | |
| `task` | type, opportunity_id, question_id, dedupe_key (unique), priority, state, lease_until, attempts, budget, inputs (jsonb), outputs (jsonb), error | |
| `llm_call` | task_id, model, prompt_version, input_tokens, output_tokens, cost_usd, latency_ms, status | |
| `budget_ledger` | scope (global/source/opportunity), period, units/usd_used, cap | |

## 9. Non-functional requirements

| ID | Requirement |
|---|---|
| NFR-1 **Durability** | All state in Postgres; workers stateless; leased tasks recover after crash |
| NFR-2 **Idempotency** | Connectors (cursor + hash), tasks (dedupe key), actions (idempotency key) |
| NFR-3 **Traceability** | Every displayed claim links to a verified span in a stored snapshot; every number links to its factor inputs |
| NFR-4 **Reproducibility** | Model, prompt version, and input hash stored for all LLM outputs; re-run any task type over stored inputs |
| NFR-5 **Security** | Untrusted-content readers have no external-write capability or credentials; structured outputs only; egress denylist; secrets never in prompts |
| NFR-6 **Privacy** | Minimal personal data; author identifiers hashed; prospects are organisations; retention policy for uncited raw documents |
| NFR-7 **Compliance** | ToS register gating; no bypassing auth, paywalls, anti-bot measures |
| NFR-8 **Cost control** | Daily caps enforced before admission; cost per card and per advanced opportunity reported |
| NFR-9 **Model independence** | Thin LLM interface; per-task-type model config. Claude for every role by founder decision; the interface still permits another provider later |
| NFR-10 **Operability** | Single-host `docker compose` deployment (Postgres, worker, scheduler); one-command backup |

## 10. Acceptance criteria

**Phase 0 → Phase 1 gate:** founder blind-prefers structured cards to B0 in ≥ 4/5 pairs (or documents why not), and templates, gates, and tiers are frozen as v1.

**Phase 1 acceptance (operation weeks 4–8):** A1–A10 from `06` §3: citation validity 100%; claim support ≥ 90%; dedupe P ≥ 0.9 / R ≥ 0.7; duplicate cards ≤ 10%; usefulness ≥ 40%; baseline win ≥ 65%; ≥ 1 opportunity to Validating with prospect list; founder time ≤ 60 min/week; 4 consecutive unattended weekly cycles; costs reported.

**Engine stop rule (pre-committed):** at week 8 of operation, if baseline win rate < 55% or zero opportunities reached Validating, stop engine infrastructure work and switch to founder-led vertical exploration, using the pipeline as a research assistant.

## 11. Metrics (reported)

- **Integrity:** span validity, entailment pass rate, spot-check precision, independence ratio
- **Discovery:** distinct verified problems/week, novelty distribution, duplicate-card rate, source yield
- **Decision:** usefulness rate, baseline win rate, advance rate (card → Validating), median days Qualified → Validating
- **Validation:** experiments run, founder hours per experiment, Brier per factor, optimism bias
- **Commercial:** time to first dollar, MRR, retention, margin, realised vs. expected value per founder-hour
- **Cost:** $ and tokens per card, per advanced opportunity, per validated opportunity

## 12. Risks

| Risk | Mitigation in this PRD |
|---|---|
| Engine becomes the product | Phase gates; pre-committed stop rule; week-9 go/no-go |
| Fabricated or inflated evidence | EXT-2 span verification; RES-3 entailment; EXT-4 rule-based tiers |
| Correlated agent optimism | Blinded Bull/Bear debate with a fresh Judge; dual research; calibration shrinkage; Bear hit rate tracked |
| Public-signal bias toward crowded markets | Source mix weighted to structured spend-revealing lenses |
| Distribution in the abstract | M3 prospect-list gate; First-10 plan; founder-executability checks |
| Loops and waste | Question-linked tasks, dedupe keys, earned budgets, saturation stopping, back-off |
| ToS/legal exposure | ToS register gating; no restricted collection; founder gates F1–F4 |
| Prompt injection | Capability separation; structured outputs; injection flags |
| Founder overload | Attention caps; ≤ 3 cards; portfolio limits |
| "Unlimited" inference not really unlimited | Budget ledger and caps from day one |

## 13. Decisions required from the founder before Phase 1

1. **Language:** Python (recommended for ecosystem) or Go (review fluency). See `04` §7.
2. **Which app marketplace** to use as the Phase 1 marketplace source (pick one with permitted access and a segment of interest).
3. ~~**Employment check**~~: resolved. The founder doesn't need it, so the F1 gate is disabled in the founder profile.
4. **Model access mechanism:** confirm whether usage is metered API or a subscription plan, and whether that plan permits unattended automated use at the intended volume.
5. **Budgets:** daily caps for LLM and paid APIs.
6. **Red-team provider:** whether to use a second model provider for adversarial review.
7. **Stop rule:** explicitly accept the week-8 engine stop rule.

## 14. Proposed repository layout (Phase 1)

```
aoe/
  connectors/        # one module per source + ToS register entries
  pipeline/          # extract, verify, normalise, embed, cluster, adjudicate
  decide/            # triage, gates, factors, montecarlo, assess, rank
  research/          # agent loop, tools (search, fetch_and_snapshot, kb_search)
  scheduler/         # queue, admission control, budgets, saturation
  report/            # digest + card templates
  cli/               # aoe command
  prompts/           # versioned prompt files per task type
  eval/              # labelled pairs, gold sets, baseline runner, backtests
  migrations/
docs/                # this directory
config/founder.yaml
docker-compose.yml
```
