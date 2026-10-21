# 08 — Operational Model for Continuous Autonomous Discovery

> Principle: **continuous ≠ busy.** The system runs on a calendar built around one scarce resource, the founder's weekly review hour, and on events that actually change decisions.

---

## 1. Cadence

| When | Job | Notes |
|---|---|---|
| **Per source cadence** (default daily; weekly for slow sources; static datasets on new release) | Collect → snapshot → extract → verify → embed | Back-off doubles the interval when N consecutive runs yield no new verified observations |
| **Event-driven** (minutes–hours) | Change monitors: pricing-page diffs, regulatory feed items, platform changelogs, shutdown notices | Only triggers research if the event matches an active segment, tracked competitor, or parked wake condition |
| **Nightly** | Attach observations to problems; triage new problems; scheduler regenerates task queue; research tasks run within budget; Assessor updates; wake-condition checks | Bounded by daily token/$ caps |
| **Weekly (before review)** | Re-cluster; pairwise ranking of eligible opportunities; baseline B0 cards; **digest generation** | Digest ready at a fixed time the founder chooses |
| **Weekly (founder, ≤ 60 min)** | Review ≤ 3 cards; blind pairwise; spot-check 3 findings; approve/deny queued actions; set priorities, pauses, budgets | All via CLI + markdown, or chat buttons |
| **Monthly** | Source-yield report; reason-code mining; calibration update; inter-run consistency check; B1 founder-manual baseline (optional) | 15-minute read |
| **On approval** | Validation experiments, prototype work | Time-boxed and budget-capped per item |

The v1.0 "every 4 hours" collection is replaced by **per-source cadences and event triggers**. Very few sources change in decision-relevant ways at sub-daily frequency.

---

## 2. The founder's weekly hour

The digest is designed to be **finished in ≤ 60 minutes**.

```
WEEKLY DIGEST — 2026-W43
─────────────────────────────────────────────────────────
0. System health (1 line): ✓ 4/4 sources healthy · $X spent · 0 blocked
1. Decisions needed (approval queue)          [≤ 5 items, ~10 min]
2. Top opportunity cards                      [≤ 3 cards, ~30 min]
3. Notable changes to tracked opportunities   [≤ 5 bullets, ~5 min]
4. Blind pairwise: AOE vs baseline            [2 pairs, ~5 min]
5. Spot-check: 3 random findings              [~3 min]
6. Parked → woken this week                   [list]
7. Killed this week (with reason codes)       [list, skim]
─────────────────────────────────────────────────────────
```

### Opportunity card (fixed template)

```
## [Segment] — [Job] — [state]                         card id · week first seen
Problem (normalised):  <actor> needs to <job> but <obstacle>, causing <consequence>
Best evidence (tier, independent origins):
  • T1 — "<verbatim quote>" — source, date  [snapshot link]
  • T3 — "<verbatim quote>" — source, date
  • T4 ×7 independent orgs (top 2 quotes linked)
Counter-evidence: • "<quote>" — source
Gates: F1–F4 ✓ · M1 ✓ · M2 ✓ · M3 ✓ (63 prospects) · M4 ? · M5 ✓ · M6 ✓
Paths:  service-first (TTFD ~3 wks) · plugin on <marketplace> (TTFD ~8 wks)
12-mo MRR:  P10 $0 · P50 $1.4k · P90 $6k   ·  P(≥$1k)=0.52  P(≥$5k)=0.14
Weakest link: p_pay at $300/mo (evidence: T7 only)
Debate: strongest surviving Bear objection: "<objection + evidence>" (Judge: Bear holds)
Next experiment: 8 problem interviews from prospect list; kill if <2 report current spend ≥ $150/mo
  Founder cost: ~4 h · $0 · [approve] [defer] [kill: reason]
```

Decisions are recorded in one keystroke plus an optional reason code.

---

## 3. Human-attention budget (hard limits, configurable)

| Limit | Default |
|---|---|
| Cards per weekly digest | 3 |
| Approval items per week | 5 |
| Concurrent validations | 2 |
| Concurrent builds | 1 |
| Ad-hoc notifications outside the digest | Only for: approval with a deadline < 72h, budget exhaustion, system failure > 24h |

If more items qualify, the system ranks them and holds the rest. It doesn't push them through. **A week with zero cards is a valid outcome**, and the digest says so explicitly ("No opportunity met the bar this week. Closest: X, blocked on Y.").

---

## 4. Resource controls

| Resource | Control |
|---|---|
| LLM tokens | Daily cap + per-task cap + per-opportunity earned budget; `llm_call` ledger |
| Paid APIs (search, data) | Daily $ cap; per-source $ cap; alerts at 80% |
| Browser sessions | Max concurrent; only for sources marked browser-permitted |
| Storage | Snapshot retention policy (keep cited snapshots forever; uncited raw documents expire after N days) |
| Compute | Worker concurrency setting; sandbox CPU/mem/time limits |
| Retries | Max 2; dead-letter after |
| Research depth | Proposal depth ≤ 2; saturation stopping (`04` §5) |

---

## 5. Failure handling and observability

- **Source health**: last success, last new observation, error rate, ToS status. Shown in the digest only when something is unhealthy.
- **Dead letters**: failed tasks with error class; the digest shows the count, with details available via CLI.
- **Drift alarms**: extraction rejection rate spike, entailment pass rate drop, duplicate-card rate rise, cost per card rise > 2× week-over-week.
- **Idempotency**: tasks have deterministic dedupe keys; actions carry idempotency keys; connectors use cursors and content hashes so a re-run produces no duplicates.
- **Restarts**: all state in Postgres; workers are stateless; a crash mid-task leaves the task `running` with a lease that expires and the task is retried.

---

## 6. Governance

- **ToS register** per source: access method, terms reviewed date, allowed uses, rate limits. A connector can't be enabled without an entry.
- **Approval-required actions** (unchanged from v1.0 §13, enforced by the Action executor): any outreach, public posting, purchase, domain registration, deployment, publication, third-party repo submission, contractual commitment, handling sensitive customer data.
- **Audit log**: every decision and action with actor, inputs, and outputs.
- **Kill switch**: one command pauses all admission and outbound actions.
