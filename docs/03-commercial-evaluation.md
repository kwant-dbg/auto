# 03 — Commercial Evaluation Framework

> Principle: **an opportunity is a bundle of conjunctive bets.** The evaluation's job isn't to produce a score. It should (1) kill opportunities fast, (2) show which bet is weakest, and (3) choose the cheapest experiment that would change the decision.

---

## 1. Why the v1.0 scoring model has to change

v1.0 proposes a weighted additive score: WTP 20, severity 20, acquisition 20, technical 15, retention 10, differentiation 10, unit economics 5.

Problems:

1. **Wrong functional form.** A business needs demand **and** reach **and** delivery **and** margin. Additive scores let a strong demand score hide a fatal distribution score. Example: WTP 20 + severity 20 + acquisition **0** + technical 15 + retention 10 + differentiation 10 + economics 5 = **80/100** for an opportunity that cannot be sold.
2. **False precision.** LLM-assigned points on a 0–20 scale carry roughly ±5 points of noise across runs. The ranking among the top ten is mostly noise.
3. **Attractiveness and evidence are mixed together.** v1.0 notices this ("confidence must be separate"), but the score itself doesn't use confidence.
4. **It doesn't guide action.** A score of 72 doesn't tell you what to do next. The real question is: *which uncertainty, if resolved, would most change our decision, and what does resolving it cost?*

---

## 2. The replacement: three layers

```
Layer 1  GATES        binary pass / fail / unknown        → kill, research, or continue
Layer 2  FACTORS      ranged estimates + evidence tier    → EV range, weakest link
Layer 3  RANKING      value of next experiment + pairwise → what to work on / show the human
```

### 2.1 Layer 1: Gates

Each gate returns **PASS / FAIL / UNKNOWN**. FAIL kills the opportunity (or parks it with a wake condition). UNKNOWN creates a research question. It is **never** a rejection by itself, which follows v1.0's point that missing evidence should prompt investigation.

**Founder gates** (configured once in the founder profile):

| Gate | Fails when |
|---|---|
| F1 Employment conflict *(optional; disabled in the current founder profile)* | Overlaps employer's business or would plausibly use employer IP or confidential knowledge |
| F2 Regulated practice | Requires a professional licence or creates professional-advice liability (legal, medical, tax advice, securities) the founder can't carry |
| F3 Solo-operability | Needs 24/7 human on-call SLAs, enterprise security reviews (e.g., SOC 2) before the first sale, or a sales cycle > ~60 days, *for the first 10 customers* |
| F4 Ethics/ToS | The product depends on violating platform terms, scraping restricted data, or deceptive practices |

**Market gates:**

| Gate | PASS requires |
|---|---|
| M1 Identifiable buyer | A buyer *role* with budget authority in a *specific segment* (not "SMBs") |
| M2 Spend evidence | ≥ 1 observation at evidence tier T1–T3 (see §3) that this buyer type spends money on this job today: on labour, services, or software |
| M3 Reachability (prospect-list test) | The engine produced ≥ 50 named organisations in the segment, each with a public contact path, **and** ≥ 1 channel the founder can personally execute |
| M4 Not obviously solved | No incumbent with a strong rating that serves this exact segment at a comparable price, *or* a clear wedge against it |
| M5 Margin possible | Rough unit cost (inference + infra + support) < 40% of the plausible price, under base-case assumptions |
| M6 Feasible now | No critical dependency on a capability or integration that doesn't exist or is prohibited by terms |

### 2.2 Layer 2: Factors as ranges

Estimate each factor as **(low, base, high)** with an **evidence tier** and **citations**. Keep the set small. Every factor must be something a future experiment can measure.

| Factor | Unit | Notes |
|---|---|---|
| `N_reachable` | orgs | Buyers reachable through the *named* channel in 12 months. **Not** TAM. |
| `price` | $/mo (or per-unit) | Anchored to observed alternatives: labour cost of the task, agency price, incumbent price |
| `p_problem` | probability | P(the problem is as described for this segment) |
| `p_pay` | probability | P(a qualified prospect pays at `price` after seeing a working solution) |
| `p_reach` | probability | P(the founder gets a qualified conversation with a prospect from the list), per attempt |
| `p_build` | probability | P(the critical workflow can be delivered at acceptable quality and cost) |
| `retention_12m` | fraction | Expected logo retention; recurring-job frequency is the main driver |
| `unit_cost` | $/customer/mo | Inference + infra + third-party + support time × hourly value |
| `founder_fit` | multiplier 0.5–2.0 | Credibility, network, domain knowledge, interest |
| `time_to_first_dollar` | weeks | Includes the service-first path |
| `defensibility_24m` | ordinal 1–5 | Survives better general agents, incumbent copying, and OSS? (see §6) |

**Derived quantities** (computed by code, not by the LLM):

- `customers_12m ≈ N_reachable × p_reach × p_pay × founder_fit-adjusted conversion`
- `MRR_12m ≈ customers_12m × price × retention-adjusted factor`
- `EV ≈ p_problem × p_build × MRR_12m × margin`
- **Weakest link** = the factor with the largest contribution to EV *variance*, computed by simple one-at-a-time sensitivity: low vs. high on each factor, others at base.

Run a **small Monte Carlo** (triangular distributions over (low, base, high), ~10k draws, milliseconds of compute). Report **P10 / P50 / P90 MRR at 12 months** and **P(MRR ≥ $1k)** and **P(MRR ≥ $5k)**. This is how financial uncertainty is represented (v1.0 open question 11): ranges in, distributions out, and no single-point forecasts.

**Honesty rule:** a factor whose evidence is only T6–T7 (commentary or model inference) gets its range widened automatically to a fixed wide prior. The model can't narrow a range without citing evidence.

### 2.3 Layer 3: Ranking

Rank opportunities by **what to do next**, not by attractiveness.

1. **Value of the next experiment.** For each opportunity, take its open critical questions (see `05-lifecycle.md`). For each question, compute roughly:
   `VoE ≈ P(result flips the decision) × |EV(pursue) − EV(stop)| ÷ cost_of_experiment (in founder-hours + $)`.
   Approximate this with simple heuristics. Precision isn't the point; ordering is.
2. **Pairwise LLM comparison** for the human-facing top list. Comparing two dossiers side by side ("Which is more likely to reach $5k MRR within 12 months for *this* founder? Cite the deciding evidence.") is more reliable than absolute scoring. Aggregate with a Bradley-Terry / Elo fit over ~3× N comparisons, randomise presentation order, and run each comparison in both orders to cancel position bias.
3. **Human preferences as ground truth.** Each weekly review produces founder pairwise choices and reason codes. These are stored and used (a) as few-shot exemplars for the comparison prompt and (b) to measure agreement between AI and founder rankings over time.

The **top of the human queue** is: opportunities that pass all gates, have high P(MRR ≥ $1k), and have a cheap, decisive next experiment.

---

## 3. Evidence-tier ladder

All evidence is classified at extraction time and re-checked by the verifier.

| Tier | Name | Examples | Weight |
|---|---|---|---|
| **T0** | Commitment to *us* | Payment, signed LOI with $, pre-order, paid pilot | Decisive |
| **T1** | Observed third-party spend | Public contract award; paid-app installs/reviews; template sales counts; disclosed revenue of a comparable business; salary band × task share | Strong |
| **T2** | Direct statement *to us* | Interview statement of current spend/time; founder's own first-hand experience | Strong if specific |
| **T3** | Published offer | Price lists of agencies, incumbent pricing pages, freelance gig listings (supply, not demand) | Moderate (proves a market *can* exist) |
| **T4** | Specific organic complaint | Names actor, frequency, cost, tool, failure | Moderate per independent org |
| **T5** | Engagement / volume | Search volume, upvotes, stars, follower counts | Weak |
| **T6** | Commentary | Blogs, analyst opinion, "industry reports" without methodology | Weak |
| **T7** | Model inference | Any LLM estimate without a citation | **Not evidence**; shown separately as "assumption" |

Rules:

- **Independence counting.** Evidence counts once per *independent origin*. Same author, same organisation, syndicated copies, and quote chains collapse to one origin. Use near-duplicate text detection and canonical-URL/author/org matching.
- **Freshness decay.** Each tier has a half-life per source type, e.g., pricing pages 6 months, complaints 12 months, procurement awards 24 months, O*NET 36 months. Stale evidence still counts, but less, and is flagged.
- **Source reliability prior.** Each source type starts with a prior. The prior is updated whenever the verifier or the human finds an error traced to that source.
- **Counter-evidence is first-class.** Every factor shows supporting *and* contradicting citations.

---

## 4. Estimating each commercial quantity: AI-doable vs. needs external verification

This section answers the question of where AI-based analysis is reliable and where external verification can't be avoided.

| Quantity | What AI can do reliably | What AI cannot do (requires external verification) |
|---|---|---|
| **Real demand** | Find and count independent T1–T4 signals; normalise and cluster | Confirm the signal represents *current buying intent* for *your* offer → interviews, concierge offer |
| **Customer spending** | Locate public spend evidence (contracts, prices, salaries × task share, install counts) | Actual per-customer budget and who controls it → interviews ("what did you pay last month for this?") |
| **Willingness to pay** | Anchor price ranges to alternatives (labour, agency, incumbent) | WTP for *your* offer. Only an ask for money measures it → pre-sale, paid pilot, priced landing page with real checkout |
| **Competitive pressure** | Enumerate competitors (search, marketplaces, directories); pricing teardowns; review-sentiment summaries | Hidden competitors (offline, regional, in-house tools); switching friction → interviews ("what do you use now?") |
| **Distribution difficulty** | Build prospect lists; find communities, directories, marketplaces; estimate list sizes | Response rates and conversion → a small, approved outreach batch (e.g., 30 personalised messages) |
| **Operating economics** | Model inference/infra cost precisely; benchmark task cost on sample inputs | Support burden, edge-case rate on *real* customer data → concierge/pilot |
| **Revenue potential** | Monte Carlo over the factor ranges | Real conversion and churn. Only time reveals them |
| **Defensibility** | Structured analysis (§6); check for OSS and general-agent substitutes | Whether customers *value* the moat (e.g., do they care about the integration?) → interviews |

**Inherently unreliable for AI, with no workaround:** WTP, conversion rates, churn, and the founder's own execution capacity. The system must label these as *assumptions to be tested*, never as findings.

---

## 5. Distribution feasibility: making it concrete

For every opportunity that passes gates M1–M2, the engine must produce a **First-10 Plan**:

1. **Prospect list** (≥ 50 named organisations), from public directories, licence registries, association member lists, marketplace vendor pages, procurement buyer lists, or ATS job boards. Store each prospect with: org name, segment fit evidence, public contact path, and the observation that qualifies them.
2. **Channel** (exactly one primary, one backup), chosen from: marketplace listing; founder-led outbound; a community where the founder can contribute credibly; partnership with an agency or consultant who already serves the segment; integration listing on a platform; SEO for a narrow high-intent query (only if search evidence exists).
3. **Founder-executability check**, phrased as questions the founder answers in the review:
   - Can I write a credible first message to this buyer today?
   - Do I know anyone within one hop of this segment?
   - Is this channel something I can do for 3 hours/week for 8 weeks?
4. **Expected funnel** with ranged rates (from base rates, labelled as T7 assumptions until measured): messages → replies → calls → pilots → paid.
5. **Kill threshold**, pre-committed: e.g., "< 3 replies from 40 personalised messages → re-examine segment/message; < 1 call from 80 → kill channel."

**ACV guidance for a founder without distribution:** prefer offers where **10–25 customers reach $5k MRR**, i.e., roughly **$200–$500/mo**, unless a marketplace or other existing channel makes the 100+ customer path realistic. Under $50/mo, require a built-in distribution channel (marketplace, platform integration, strong search intent).

---

## 6. Defensibility over a 24-month horizon

v1.0 asks the right questions. Here they are as an ordinal rubric, scored 1–5 with a citation-backed rationale:

| Question | Defensive if… |
|---|---|
| Will a general-purpose agent do this for the customer in 18–24 months? | The product owns **system-of-record data**, **scheduled/unattended execution**, **integrations with write access**, or **accountability** (audit trail, compliance artefacts) that a chat agent doesn't provide |
| Will the platform/incumbent build it? | It serves a segment too small for the incumbent, or spans multiple platforms that won't integrate each other |
| Will OSS commoditise it? | The value is in reliability, hosting, support, domain-specific QA, or managed data, not in the algorithm |
| Does it get better with use? | Lawful, consented accumulation of workflow templates, edge-case libraries, or benchmark data per vertical |
| Is there trust or switching cost? | Embedded in recurring operations (month-end close, compliance filing) with history the customer relies on |

**Red-flag pattern:** "a prompt + a UI over a single model call" scores 1 on defensibility regardless of current demand. That doesn't make it a fatal gate, because a fast cash-flow product can still be worth building. But it must be **labelled as a short-lived product** in the business case.

---

## 7. Pricing model selection

Don't default to SaaS. Pick the model whose *unit of value* matches the customer's *unit of pain*:

| If the customer's pain is… | Prefer |
|---|---|
| A recurring deliverable (monthly report, reconciliation, filing) | **Per-deliverable or flat monthly**, tied to the cadence |
| Volume-driven work (documents, tickets, records) | **Usage-based with a floor** |
| A deadline-driven obligation | **Per-filing / per-event** + optional annual plan |
| Messy, judgment-heavy, early-stage | **Service-assisted** (done-for-you at agency-undercut price), productised later |
| A capability inside a platform | **Marketplace app pricing** conventions of that platform |

---

## 8. Business-model paths evaluated side by side

For every opportunity that clears the gates, the engine evaluates **at least three paths**:

1. **Service-first:** the founder delivers the outcome manually, using internal AI tooling. Revenue in weeks, highest learning, doesn't scale; it's a bridge.
2. **Narrow software:** SaaS, plugin, or API for the critical workflow.
3. **General-purpose tool:** a horizontal product for a job that recurs across many industries (e.g., the same reconciliation task in accounting firms, dental offices and agencies). It's a first-class path. It needs a distribution channel that works across segments (search intent, an integration ecosystem, open source), and gate M3 checks for one.
4. **Distribution-first:** a marketplace/platform plugin whose discovery is driven by the platform.
5. *(Optional)* **Acquire:** an existing small product in the space, if listings exist.

Each path gets its own `time_to_first_dollar`, `p_build`, and channel. The recommendation names the path, not just the opportunity.

---

## 9. Calibration

Calibration is impossible at first, because no outcomes exist yet. Build it in stages:

1. **Weeks 0–8: founder agreement.** Compare the AI pairwise ranking with the founder's choices (Kendall τ on weekly top-10). Low agreement → fix prompts and evidence, *not* the founder.
2. **Months 2–6: experiment outcomes.** Every validation experiment ships with **pre-registered predictions** (e.g., "P(≥ 3 of 10 interviewees report current spend ≥ $200/mo) = 0.6"). Score these with **Brier scores**, per factor type. Systematic optimism (the usual result) → apply a learned shrinkage factor to future `p_*` estimates.
3. **Months 6+: commercial outcomes.** Realised MRR vs. P10/P50/P90 bands. With so few products, this is anecdotal. Treat it as a sanity check, not a training signal.
