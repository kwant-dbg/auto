# 06 — MVP Design and Phased Roadmap

> Principle: **prove the engine beats a $5 prompt before building the engine.** Each phase has a hypothesis, a time box, measurable exit criteria, and a pre-committed *stop-building* condition.

---

## 1. The smallest thing that demonstrates real value

Real value = **the founder takes an action they wouldn't otherwise have taken, on an opportunity they wouldn't otherwise have found, backed by evidence they trust.** Concretely: an approved validation experiment on an AOE-sourced opportunity.

The smallest system that can produce that outcome:

1. Three to four **high-signal sources** (not ten).
2. Extraction with **verbatim span verification**.
3. Normalised clustering into problems.
4. Triage gates + a **question-driven research loop** on the top few candidates.
5. A **weekly digest of ≤ 3 opportunity cards** with First-10 plans.
6. A **feedback capture** step (founder rankings + reason codes).
7. A **baseline comparison**, run from week 1.

Everything else waits.

---

## 2. Phase 0: Wizard-of-Oz (1 week, ~no infrastructure)

**Hypothesis:** a structured, evidence-first research process produces opportunity cards the founder prefers to a one-shot prompt.

**Build:**

- A repo of **prompt files** (extractor, assessor, red team, card template) and a founder-profile file.
- Run them by hand with a coding agent / chat interface against **manually chosen** inputs: one O*NET-based seed list, one app-marketplace category, one procurement query.
- Output: 5 opportunity cards as markdown, each with citations to saved page snapshots.
- In parallel: generate 5 cards with **baseline B0** (one-shot deep-research prompt).
- Founder completes the **self-interview** (`02` §3.9).
- Hand-label ~200 observation pairs as same/different problem (for dedupe calibration later).

**Exit criteria:**

- Founder blind-prefers AOE-style cards to B0 cards in ≥ 4 of 5 pairs, **or** identifies specifically why not. That reason then reshapes Phase 1.
- Card template, gate definitions, and evidence tiers are stable enough to encode.

**What this gives you:** the rubric, the templates, the baseline, and a reality check, all before writing infrastructure.

---

## 3. Phase 1: Discovery MVP (4–5 weeks)

**Hypothesis:** an automated pipeline over a few structured, spend-revealing sources produces ≥ 1 validation-ready opportunity per month at ≤ 60 min/week of founder time.

### Scope

| Area | In scope |
|---|---|
| Sources | (1) **O*NET + BLS OEWS + AI-usage index** static import and seed generation; (2) **one app marketplace** with an official API or permissive terms; (3) **public procurement** API (contract awards); (4) **Hacker News + GitHub issues** search for corroboration queries only. *(Optional 5th: ATS public job boards for a curated company list.)* |
| Storage | Postgres + pgvector; content-addressed snapshot files |
| Pipeline | Connectors → Extractor → span verifier → normaliser/embedder → clusterer (ANN attach + LLM adjudication) → problems |
| Decide | Triage gates → opportunities → questions → scheduler → Researcher (web search + fetch/snapshot + KB search) → verifier → Assessor (factor ranges + Monte Carlo) → Debate (Bull / Bear / Judge) |
| Human | Weekly markdown digest (≤ 3 cards + "notable changes" + "blocked"), committed to a private repo + notification; CLI to rate cards, rank pairwise, record decisions with reason codes, set wake conditions |
| Control | Postgres queue; nightly and weekly cron; budgets; dedupe keys; saturation stopping; dead-letter table |
| Eval | Baseline B0 cards generated weekly; blind pairwise review in the digest; 3 random findings to spot-check per week |

### Explicitly **not** in Phase 1

Dashboard UI (use Metabase if needed), Temporal, prototype builder, any outreach automation, more than one LLM provider, freelance-marketplace or large social-platform connectors, revenue modelling beyond the Monte Carlo, graph DB, Kubernetes, multi-user support, landing-page tooling.

### Build sequence (each step is demoable)

| Week | Deliverable | Demo |
|---|---|---|
| 1 | Schema + migrations; snapshot store; 2 connectors (O*NET/BLS import, procurement API); Go `claude` runner with auth health check, plus a check that WebSearch works headless under the subscription; `llm_call` logging; capacity caps | SQL: tasks × industries × wage pool; 1k procurement awards stored |
| 2 | Extractor + span verifier + normaliser + local embedding server; Atlassian Marketplace connector; `aoe-mcp` tool server (`fetch_and_snapshot`, `kb_search`); HN/GitHub search | Observations with verified spans; extraction precision on 50 hand-checked samples |
| 3 | Clusterer + adjudicator (thresholds from Phase 0 labels); triage gates; opportunities + questions; scheduler with dedupe + budgets | Problem list with independence counts; dedupe P/R report |
| 4 | Researcher agent (read-only tools, snapshot-before-cite); verifier (span + entailment); Assessor + Monte Carlo + weakest-link; Bull/Bear debate + Judge | One full dossier generated unattended |
| 5 | Digest + CLI feedback + pairwise ranking; baseline B0 job; saturation stopping; source back-off; first real weekly cycle | Founder's first weekly review; metrics report |

### Acceptance criteria (measured over weeks 4–8 of operation)

| # | Criterion | Target |
|---|---|---|
| A1 | **Citation validity**: share of displayed quotes that pass span check | 100% (hard: unverifiable quotes are never shown) |
| A2 | **Claim support**: share of spot-checked findings the founder grades "supported" | ≥ 90% |
| A3 | **Dedupe quality** on labelled set | Precision ≥ 0.9, recall ≥ 0.7 |
| A4 | **Duplicate surfacing**: cards in digest that the founder marks "same as previous" | ≤ 10% |
| A5 | **Usefulness**: cards rated "worth 30 more minutes" by founder | ≥ 40% |
| A6 | **Beats baseline**: blind pairwise win rate vs. B0 | ≥ 65% |
| A7 | **Actionability**: opportunities reaching *Validating* with a ≥ 50-org prospect list | ≥ 1 in 4 weeks |
| A8 | **Human cost**: founder time per week on review | ≤ 60 min |
| A9 | **Reliability**: weekly cycle completes with no manual intervention | 4 consecutive weeks |
| A10 | **Cost visibility**: $ and tokens per card and per advanced opportunity, reported | Reported (no target yet) |

### Stop / pivot condition for the engine itself

After **8 weeks** of operation: if A6 < 55% **or** A7 = 0, **stop investing in engine infrastructure**. Instead, either (a) choose a vertical manually from the best evidence collected so far and go founder-led, using the existing pipeline as a research assistant, or (b) radically change sources. Write this rule down before starting, because it is the main defence against "the engine becomes the product".

---

## 4. Phase 2: Commercial intelligence (4 weeks; starts only if Phase 1 passes)

**Hypothesis:** deeper, path-aware business cases with independent verification raise the founder's advance rate and improve validation hit rates.

**Add:**

- Business-model paths per opportunity (service-first / narrow software / distribution-first / acquire).
- Prospect-list builder as a first-class tool (directories, licence registries, association lists, marketplace vendor pages) with dedupe and segment-fit evidence.
- Dual independent research for opportunities entering validation; disagreement → questions.
- Change-event monitors (pricing-page diffs for tracked competitors, regulatory feed, platform changelogs) feeding wake conditions.
- Paid templates / productised services lens (sampled, manual-rate, respecting terms).
- Vertical-level assessment and explore/exploit allocation (bandit over sources/strategies).
- Optional: ATS job-board connector across a curated company list.

**Exit criteria:** advance rate (cards → Validating) improves over Phase 1; dual-research disagreement rate is tracked and resolved; ≥ 2 opportunities validated or killed *by experiment* (not by opinion).

---

## 5. Phase 3: Validation engine (4–6 weeks; overlaps real validation work)

**Hypothesis:** system-prepared experiments reduce founder hours per experiment by ≥ 50% and produce calibrated predictions.

**Add:**

- Experiment entity with pre-registered predictions, thresholds, budgets.
- Approval queue + deterministic action executor (send approved emails via the founder's own account/API, create approved landing pages, issue payment links). All actions are idempotent with dedupe keys.
- Ingest of results: interview transcripts → T2 observations; replies/payments → T0/T1.
- Brier scoring + optimism shrinkage applied to future estimates.
- Consider Temporal here if multi-week human-signal workflows proliferate (see `04` §6).

**Exit criteria:** ≥ 3 experiments completed with results recorded; founder hours per experiment measured and reduced; calibration report exists; ≥ 1 opportunity has T0 evidence (money or signed commitment), or the decision to pivot strategy is documented.

---

## 6. Phase 4: Prototype & product development (time-boxed per opportunity)

**Entry:** an opportunity reaches *Building* (behavioural validation thresholds met).

**Add:**

- Builder agent in a sandbox: repo scaffold, tests, a benchmark harness for the **riskiest technical assumption** first (e.g., extraction accuracy on 50 real documents at target cost).
- Independent code review by a second agent + founder review.
- Human-controlled deployment.
- Product metrics ingest (signups, activation events, payments, support tickets).

**Rule:** a prototype tests **one** assumption. The first customer-facing version is the smallest thing a paying pilot customer will use.

---

## 7. Phase 5: Portfolio management (only once ≥ 1 product is live)

- Explicit **portfolio limits**: ≤ 1 product in active build, ≤ 2 live products in growth, ≤ 2 validations concurrently. Human attention is the binding constraint.
- Learning loop: which sources, verticals, and paths produced revenue; reallocate discovery accordingly.
- Kill/harvest rules for live products (retention, margin, support-load floors).

---

## 8. Timeline summary

```
Week:   0    1    2    3    4    5    6    7    8    9   10   11   12   13   14   15   16
        [P0 ]
             [========== Phase 1 build ==========]
                                 [===== Phase 1 operation & measurement =====]
                                                            ▲ go/no-go (week ~9)
                                                  [====== Phase 2 ======]
                                                                [===== Phase 3 … (real validations) ...
```

By **week ~9** you should know whether the engine works. By **week ~16** you should have spent real founder hours on at least two customer-facing validation experiments. If neither has happened, the engine is not the problem to work on.
