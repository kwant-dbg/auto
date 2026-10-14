# 02 — Discovery Methodology

> Principle: **discover where money already moves, and where you can distribute.** Prefer sources that show *spend* and *reachability*. Use frustration only as a supporting signal.

---

## 1. Why the v1.0 discovery strategy needs reweighting

v1.0 lists six discovery strategies, which are all reasonable, but it gives them equal standing and leans toward online complaint channels. Those channels have three structural problems:

1. **Everyone can see them.** Opportunities visible on Reddit, HN, or Product Hunt are already being mined by thousands of founders and by commercial "pain-point" tools.
2. **They are skewed toward low-WTP segments.** Developers and indie hackers complain loudly and pay little. Back-office staff in dental practices, freight brokerages, and municipal clerk offices rarely post at all.
3. **They show frustration, not spend.** A complaint shows that a problem exists. It doesn't show that a budget exists.

The revised methodology ranks discovery lenses by **evidence of spend × reachability of buyers × how rarely others mine them**.

---

## 2. Discovery lenses, ranked

| Rank | Lens | Primary signal | Spend evidence | Distribution built in? | Mined by others? |
|---|---|---|---|---|---|
| 1 | **Labour-market decomposition** (O*NET × BLS × AI-usage indices) | Wage pools per task | Strong (aggregate) | No | Rarely joined |
| 2 | **Platform marketplace gaps** (app stores of vertical SaaS) | Installs, paid-app prices, 1–3★ reviews | Strong | **Yes** (the marketplace) | Partly |
| 3 | **Public procurement** (contract awards, RFPs) | Awarded $ for services | **Very strong** (actual payments) | Partly (public bid lists) | Rarely by indie founders |
| 4 | **Paid templates & productised services** | People paying for spreadsheets, Notion templates, and fixed-price "done-for-you" services | Strong (purchase counts and reviews) | Partly | Rarely |
| 5 | **Pay-transparent job postings** (ATS public boards) | Salary bands + task text + tools mentioned | Strong (salary) | Weak | Partly |
| 6 | **Change events** (regulation, deprecations, price hikes, shutdowns) | Deadline + affected population | Indirect, but urgent | Often (affected-user lists) | Speed-dependent |
| 7 | **Agent-skill / workflow-template ecosystems** | Popular automations, star counts, forks | Weak (popularity ≠ WTP) | Weak | Heavily |
| 8 | **Complaint & frustration mining** | Recurring specific complaints | Weak | Weak | **Heavily** |
| 9 | **Founder-proximal pain** | The founder's own SRE/platform/AI-infra experience | Variable | **Strong** (credibility) | No one else has it |

Lens 9 is listed last because it is a small, non-scalable source. It also has the **highest prior of success per opportunity**. The engine should treat any opportunity that touches founder-proximal domains as receiving a founder-fit bonus (see `03-commercial-evaluation.md`).

---

## 3. Lens details

### 3.1 Labour-market decomposition: "where are people paid to do automatable tasks?"

This is the most systematic way to answer v1.0's central question, "what are people paying other people to do?", and the least used.

**Data (all public, structured, free):**

- **O*NET**: about 900 occupations, each with detailed task statements, task importance/frequency ratings, work activities, tools and technology used, and work context.
- **BLS OEWS**: employment counts and wage distributions per occupation, broken down by industry (NAICS) and geography.
- **AI-usage indices** (e.g., the Anthropic Economic Index): AI usage mapped to O*NET tasks, split into automation-style and augmentation-style use. Treat it as a *signal of current AI adoption per task*, with its sampling caveats (one provider's users, chat-based usage).
- **BLS Employment Projections**: growth or decline per occupation.

**Method:**

1. For each (occupation, industry) cell, compute a **wage pool** = employment × median wage.
2. Spread the wage pool across tasks using O*NET importance/frequency weights. This gives an estimated **$ / year spent on task T in industry I**. The figure is crude, and that is fine; it only ranks.
3. Have an LLM score each task for **automatability with current models**, using a fixed rubric: input structure, judgment depth, error tolerance, need for human interaction, and liability. Use pairwise comparisons within an occupation, not absolute scores.
4. Join with AI-usage indices. **High wage pool × high automatability × low current AI usage = gap.** Low usage on a highly automatable task suggests a workflow-integration or trust barrier, which is exactly where a product can sit.
5. Join with industry structure, e.g., Census County Business Patterns (establishment counts by NAICS and size class). **Many small establishments** are better targets for a solo founder than a few large ones: shorter sales cycles and no procurement departments.
6. Output: a ranked list of `(task, industry, est_spend, automatability, adoption_gap, establishment_count)` tuples. Each is a **hypothesis seed**, not an opportunity.

**Why this works:** it reaches offline, boring industries that never show up in complaint mining. It starts from money already being spent, and it is a join few people bother to run.

**Limitations:** O*NET tasks are coarse and lag reality. The wage-to-task allocation is an approximation. Automatability scoring is an LLM judgment. Treat the output as a *map for choosing where to look*, then verify each seed with lenses 2–5.

### 3.2 Platform marketplace gaps: "discover where you can distribute"

Vertical and horizontal SaaS platforms run app marketplaces. Examples include e-commerce platforms, accounting/bookkeeping platforms, CRMs, project-management and dev-tool suites, website builders, and the WordPress plugin directory. Each marketplace gives you:

- **Demand with spend.** Paid-app pricing pages and install/review counts (reviews are a lower bound on paying users).
- **Specific dissatisfaction.** 1–3★ reviews of paid apps name missing features, broken integrations, and support failures.
- **Distribution.** Buyers search the marketplace directly. A plugin with a clear name for a clear job can get organic installs without a marketing budget.
- **Competitive structure.** Number of apps per category, price spread, update recency (abandoned apps with many installs = takeover opportunity).

**Signals to extract per category:** number of apps; median price; install/review distribution; share of reviews mentioning a recurring missing capability; apps with high installs and no updates for over 12 months; categories where the top app's rating is below 4.0.

**Access:** some marketplaces have official APIs (the WordPress.org plugin API and some vendor marketplace APIs). Others don't, and their terms may prohibit automated collection. **Assess each one.** Where automated collection isn't permitted, use **sampled manual research by an agent through normal browsing** at human-like volume, or skip the marketplace.

### 3.3 Public procurement: "the only public ledger of actual payments"

Government contract awards and solicitations are public records of **money actually paid for services**, with descriptions of the work.

- Federal: contract award and spending APIs (e.g., USAspending), opportunity notices (e.g., SAM.gov), both official and free with a key.
- State/municipal: procurement portals and RFP archives. These are fragmented, but small municipalities buy enormous amounts of routine services: records management, permit processing, meeting transcription, compliance reporting, translation, grant reporting.

**Method:** classify awarded contracts by *service type* (with an LLM). Cluster recurring service categories with small contract values ($5k–$100k) bought by many independent agencies. Each cluster is a recurring, budgeted workflow with a known buyer type, and it usually comes with public lists of past buyers.

**Caveats:** selling to government has long cycles and procurement rules. Treat procurement data primarily as **evidence of what services cost and that the workflow exists**. The same workflows usually exist in private-sector equivalents (e.g., HOA management, property management, private schools), which are easier to sell to.

### 3.4 Paid templates and productised services

People who pay $15–$200 for a spreadsheet template, Notion system, or "done-for-you" fixed-price service are showing that:

- the workflow is common enough to be packaged,
- the buyer has budget and has already chosen to pay rather than build it themselves,
- existing software doesn't cover it, or is too complex or expensive.

**Sources:** template marketplaces, digital-goods storefronts, and freelance marketplace *fixed-price gig* listings. On gig listings, review counts approximate completed purchases far better than listings alone. Agency and consultant **price lists** ("monthly bookkeeping cleanup for Shopify stores: $X/mo") are especially valuable because they define the deliverable precisely.

**v1.0's distinction still applies:** a *listing* is evidence of supply. Purchase evidence is *review count, "orders in queue", or sales counts*. Collect these respecting each platform's terms. Several freelance platforms have restricted automated access (e.g., one major marketplace removed its public RSS job feeds in 2024), so plan on **sampled, manual-rate research**, not bulk connectors.

### 3.5 Pay-transparent job postings

Pay-transparency laws in several US states require salary ranges in postings. Many companies publish openings through applicant-tracking systems that expose public job-board endpoints.

**Extract:** task phrases that indicate manual workflow ("manually reconcile", "compile weekly reports", "maintain spreadsheets", "chase approvals", "data entry into"), tools mentioned (a map of the incumbent software stack), salary band, company size, and industry.

**Use:** the *salary × share of the role spent on the task* gives a **per-company spend estimate** for the workflow. "Same task phrase across many unrelated small companies" is an independence-weighted recurrence signal. v1.0's caveat stands: a job posting shows demand for a *responsibility*, not for *a product*. It is a seed, and it needs further evidence.

### 3.6 Change events: "urgency arrives on a schedule"

New obligations create **deadline-driven demand with an identifiable affected population**. That is the best combination for fast sales.

| Change type | Source | Example of second-order demand |
|---|---|---|
| New regulation / reporting rule | Federal Register API, regulations.gov, EU Official Journal, state registers | New reporting format → small filers need a cheap generator / validator |
| Platform API deprecation or policy change | Platform changelogs, developer blogs, status pages | Integrators must migrate → migration tool / managed replacement |
| Price increase or plan restructure by an incumbent | Pricing-page diffs (snapshot + diff) | Price-sensitive tail customers look for a narrow alternative |
| Product shutdown or acquisition sunset | Shutdown announcements, "sunsetting" notices | Stranded customers with data to migrate and workflows to replace |
| New platform with fast adoption | Changelogs, marketplace launch announcements | Early-ecosystem gaps (plugins, ops tooling) before incumbents arrive |

**Method:** subscribe to feeds and run **diffs, not crawls**. Trigger an *event-driven research task* only when a change matches an active segment or a parked opportunity's wake condition. **Speed is the edge**: the window is weeks to months.

### 3.7 Agent-skill and workflow-template ecosystems

Popular agent skills, automation templates, and workflow repositories show **what technically capable people want automated**. Keep v1.0's caveat: popularity ≠ WTP. Use this lens mainly to find **capability unlocks**: "this used to be impossible, and now a 200-line skill does it". Then route the *task* back to lenses 1–5 to look for a paying buyer who is *not* technical enough to run the skill. That buyer would pay for a hosted, reliable, finished workflow. The developers starring the repo usually won't.

### 3.8 Complaint and frustration mining (de-emphasised but kept)

Keep it, with three changes:

1. **Use it to corroborate, not to originate.** Run targeted searches *about a seed from lenses 1–6* ("complaints about [incumbent] for [segment]"), not open-ended pain-point trawling.
2. **Count independent organisations, not posts.** Cluster by author and organisation; collapse syndicated or quoted copies.
3. **Weight by specificity.** "Ugh, invoicing sucks" ≈ 0. "We spend 6 hours every month-end re-keying Stripe payouts into QuickBooks because the sync drops refunds" is a real observation: it names actor, frequency, cost, tool, and failure mode.

Respect platform data terms. Several large community platforms restrict commercial use of their APIs and require agreements. Hacker News (official API and a public search index), GitHub (API), and Stack Exchange (subject to current data-access terms) are the most accessible for technical communities.

### 3.9 Founder-proximal pain

Run a structured **founder self-interview** once, with a guided questionnaire administered by an agent. Record its answers as evidence of tier T2 ("direct statement") with the founder as the source:

- Internal tools you or your teams built because nothing on the market fit.
- Recurring ops toil in SRE, platform, Kubernetes, Temporal, or LLM infra work that you've seen across multiple organisations.
- Vendor products you've seen teams overpay for or complain about.
- Communities where you already have credibility (talks, OSS, colleagues, ex-colleagues).

Then expand each item with lenses 2, 5, and 8 to test whether it generalises beyond your own organisations. **Check your employment agreement before pursuing anything that overlaps your employer's business or uses knowledge gained there.**

---

## 4. Creative / non-obvious discovery techniques

These are the techniques most likely to surface *overlooked* niches, not obvious startup ideas.

1. **"Spreadsheet archaeology."** Search for publicly shared spreadsheet and Notion templates (and template-marketplace listings) for specific professions. A thriving template is a product spec written by the market.
2. **Abandoned-incumbent detection.** On marketplaces: apps with many installs, recent negative reviews, and no update in over 12 months. In OSS: projects with high usage and unmaintained issue trackers. Customers are stranded and switching costs are already partly paid.
3. **The price-gap ladder.** For a workflow, chart the price of (a) doing it by hand (wage × time), (b) a freelancer or agency, (c) the incumbent enterprise software, and (d) any SMB tool. **Large gaps between rungs** (e.g., an agency at $1,500/mo with no tool between $50 and $1,500) mark price points a narrow product can occupy.
4. **"Service-to-software" tracing.** Find agencies and productised services that **have grown** (team pages, more hires, more listings). Their internal delivery process is a validated workflow. Ask: could a tool let their *customers* do it, or could it make *other agencies* 5× faster? Selling to agencies is a strong solo-founder channel: few buyers, high ACV, clear ROI.
5. **Certification and continuing-education catalogues.** What professionals must learn or re-certify on lists their recurring hard tasks. New course topics lead regulatory and tooling change.
6. **Trade association conference agendas and vendor-exhibitor lists.** Session titles show what an industry worries about this year. Exhibitor lists show what it already buys. Gaps between the two are seeds.
7. **"What does the integration list lack?"** For a vertical SaaS leader, compare its native integrations with the tools that same segment's job postings mention. Missing pairs are integration-product seeds, with a distribution channel already in place (the platform's marketplace).
8. **Second-order regulation tracing.** For each new rule: who must comply → what artefact they must produce → who currently produces it (accountant? consultant? nobody yet?) → what it costs.
9. **Capability-unlock re-scan.** When model capability or cost changes materially (e.g., document extraction accuracy, voice latency, computer-use reliability), automatically re-score **parked** opportunities whose blocker was that capability. Rejected ideas are an asset only if they are re-checked.
10. **Acquisition-marketplace mining.** Listings of small SaaS businesses for sale disclose niche, MRR, churn, customer count, and the seller's reason for leaving. They give **base rates** for what reaches $1–10k MRR in which niches, and possible **acquisition targets**.
11. **Negative space vs. adjacent geographies.** Workflows well served by local tools in one country (e.g., e-invoicing compliance in one market) but underserved in another with similar new rules.

---

## 5. Discovery pipeline: from raw document to problem

```
Source connector ──► RawDocument (snapshot, hash, timestamps)
                         │
                         ▼
          Extractor (LLM, no tools, JSON schema)
                         │  emits 0..n Observations, each with:
                         │   - verbatim span offsets into the snapshot
                         │   - normalised job statement (actor | job | obstacle | consequence)
                         │   - evidence tier, specificity score, spend hints
                         ▼
          Citation check (deterministic: span text ∈ snapshot)
                         │
                         ▼
          Embedding of the *normalised* statement (not the raw text)
                         │
                         ▼
          Clustering → Problem candidates (hierarchical; see §6)
                         │
                         ▼
          Independence counting (distinct orgs/authors/origins)
                         │
                         ▼
          Problem ranking for triage
```

**Key design choice: normalise before embedding.** Raw complaints about the same problem use very different vocabulary ("Stripe→QBO sync drops refunds" vs. "my accountant keeps asking why payouts don't match"). Rewriting each observation into a fixed template, `<actor in segment> needs to <job> but <obstacle>, causing <consequence>`, and embedding *that* makes cross-source deduplication much more reliable than embedding raw text.

---

## 6. Deduplication across unrelated sources

1. **Normalise** (above).
2. **Embed** the normalised statement. Run approximate nearest-neighbour search against existing problems.
3. **Three bands:**
   - Similarity ≥ high threshold → auto-attach to the existing problem.
   - Similarity in an ambiguous band → **LLM adjudication** with a fixed rubric: same actor? same job? same obstacle? The verdict is one of *same / parent-child / sibling / different*.
   - Below the band → new problem candidate.
4. **Hierarchy, not flat merging.** "Bookkeeping is painful for e-commerce" is a parent. "Stripe payout reconciliation with refunds in QuickBooks for Shopify merchants" is a child. Opportunities attach to *leaf or near-leaf* problems. Parents aggregate evidence for reporting.
5. **Periodic re-clustering** (weekly) with a density-based algorithm over all normalised statements, to catch drift. Diffs are reviewed by the LLM adjudicator, not applied blindly.
6. **Thresholds are calibrated** on a small hand-labelled set (≈ 200 pairs) built in MVP week 1. Report precision and recall of the dedupe step.

---

## 7. Balancing broad discovery vs. vertical depth

**Recommendation: explore, then exploit, explicitly.**

- **Weeks 1–4 (broad):** run lenses 1, 3, and 5 across many industries to produce a *vertical shortlist*. Pick 2–3 verticals using a vertical-level assessment: establishment count, wage pool, number of spend-evidenced problems, reachable channels, founder access.
- **Weeks 5+ (deep):** 80% of effort goes into the chosen verticals, where evidence, buyer lists, vocabulary, and relationships *compound*. 20% stays broad as exploration, which keeps the option to switch open.
- Revisit the allocation monthly using **source and vertical yield** (opportunities advanced per unit of effort). Allocate with a simple bandit: Thompson sampling over strategies, with reward = founder-advanced opportunities.

Depth wins because the expensive part of a business (distribution knowledge, trust, a buyer list) is vertical-specific and doesn't transfer. Breadth exists to pick the right vertical, not to build an endless idea list.
