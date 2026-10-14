# 07 — Evaluation Framework: Is the Engine Any Good?

> Principle: **outcomes are the only true metric, but they arrive too slowly and too rarely to steer by.** Use a ladder of proxies, each with known biases, and keep checking that the proxies predict the outcomes.

---

## 1. The measurement problem

- **Small N.** You'll launch a handful of products in a year. Revenue outcomes can't statistically distinguish a good engine from a lucky one.
- **Long lag.** It takes months from discovery to revenue.
- **Self-grading risk.** If the engine's own scores measure its quality, it will optimise for persuasive dossiers.

So evaluation is layered. Lower layers are fast and plentiful but further from the truth. Higher layers are slow and scarce but real.

| Layer | Question | Speed | Main bias |
|---|---|---|---|
| L1 Mechanical | Are citations real and claims supported? | Per task | None significant (deterministic) |
| L2 Research accuracy | Are findings correct and complete? | Weekly | Grader fatigue; gold-set coverage |
| L3 Human judgment | Does the founder find the cards better than baselines? | Weekly | Founder taste ≠ market; novelty bias |
| L4 Backtest | Would the engine have surfaced known good niches early? | Ad hoc | **Hindsight bias, data leakage, survivorship** |
| L5 Prediction calibration | Do the engine's probabilities match experiment outcomes? | Per experiment (weeks) | Small samples; experiment design quality |
| L6 Commercial | Revenue, retention, margin | Months | N ≈ 1–5; luck |

---

## 2. L1: Mechanical integrity (continuous)

| Metric | Definition | Target |
|---|---|---|
| Span validity | displayed quotes found verbatim in snapshot | 100% (enforced) |
| Extraction rejection rate | extractions failing span check | Track; spikes = prompt/model regression |
| Entailment pass rate | findings whose quote supports the claim (LLM check) | Track; compare to L2 human grades |
| Tier integrity | evidence tier assigned by rule matches source type | 100% (enforced) |
| Independence ratio | independent origins ÷ raw observations per problem | Track; low = syndication or one loud author |

**Limitation:** L1 proves claims are *grounded*, not *true or important*. A verbatim quote from a wrong source still passes.

---

## 3. L2: Research accuracy

1. **Weekly spot-check.** 3 random findings per digest; the founder grades each *supported / overclaimed / wrong* in 30 seconds. Report precision with a running confidence interval.
2. **Competitor-recall gold sets.** For 5–10 well-understood markets, build by hand once (2–3 hours each, or with a careful one-off research effort plus human checking) the list of real competitors. Measure the Researcher's recall. A competitor sweep that misses a well-known incumbent is the most damaging research error.
3. **Inter-run consistency.** Re-run the same dossier question with a different seed or model monthly. Measure agreement of factor ranges and gate verdicts. High variance means conclusions are noise and should be presented with wider ranges.
4. **Known-answer probes.** Seed the queue with opportunities whose answer is known (e.g., "a niche where a dominant cheap incumbent clearly exists"). The engine should kill them quickly and for the right reason.

**Limitations:** gold sets go stale and reflect the builder's knowledge. Spot-checks are noisy at 3 per week; aggregate monthly.

---

## 4. L3: Human evaluation against baselines

**Weekly blind pairwise review.** The digest includes 2 pairs: (AOE card vs. B0 card), shuffled and with source labels hidden. The founder picks the one they'd rather spend an hour on and ticks a reason code.

Baselines:

- **B0**: one-shot deep-research prompt with the same founder profile.
- **B1** (monthly): the founder's own 2–4 hours of manual research, written as cards.
- **B2** (optional): an outside founder/advisor rates a sample monthly. This reduces single-rater taste bias.

**Metrics:** AOE win rate vs. each baseline; usefulness rate ("worth 30 more minutes"); **novelty rating** ("would I have thought of this myself? 1–5").

**Novelty, measured mechanically as well (v1.0 Q10):**
- Embedding distance of each AOE opportunity from (a) a corpus of B0 outputs accumulated over time ("obvious ideas"), (b) existing product-directory descriptions, (c) previously surfaced AOE opportunities.
- Report the distribution. Novelty matters *only together with* evidence strength, because novel-and-unsupported is just fiction.

**Limitations:** founder preference is a proxy for market truth and rewards persuasive writing. Mitigations: hide scores and prose style differences by using a fixed card template for all sources (including baselines); weight usefulness ratings by later validation outcomes (L5).

---

## 5. L4: Historical backtesting

**Idea:** freeze evidence as of a past date *T* and ask whether the engine would have surfaced niches that later produced profitable small businesses.

**Construction:**

1. **Positive set.** Small software businesses (≈ $1k–$50k MRR) with public founding dates and revenue: from acquisition-marketplace listings, public "open startup" revenue dashboards, and indie-founder interviews. Record the niche and launch date.
2. **Negative set (essential).** Products that launched in similar niches and failed or stayed tiny, from shutdown posts, abandoned marketplace apps, and stalled listings. Without negatives, you measure recall only and reward an engine that flags everything.
3. **Evidence snapshot ≤ T.** Use archived web snapshots, dated marketplace reviews, dated procurement awards, dated forum posts. Filter by *publication date*, not collection date.
4. **Run the pipeline** on the ≤ T corpus. Measure: was the positive niche among the top-k problems/opportunities? What rank? Were negatives correctly ranked low or killed?

**Threats to validity:**

| Threat | Description | Mitigation |
|---|---|---|
| **Data leakage via model knowledge** | The LLM knows which niches later succeeded and will "recognise" them | Use niches that launched **after** the model's training cutoff where possible; **anonymise** niche-specific product names in the corpus; run a leakage probe (ask the model directly about the outcome; exclude cases it can answer) |
| **Hindsight bias in construction** | Positive examples are chosen *because* their early signals look obvious in retrospect | Sample positives systematically (e.g., all listings in a price band and period), not hand-picked famous cases |
| **Survivorship** | Only successes are documented | Explicit negative set; report precision, not only recall |
| **Snapshot incompleteness** | Archives miss pages | Report coverage per case; exclude cases with < X% coverage |
| **Source drift** | Today's sources/APIs didn't exist or had different terms then | Backtest only lenses whose data is historically available (procurement, O*NET/BLS vintages, dated reviews) |

**Verdict:** backtesting is useful for **catching gross failures** (the engine can't surface anything that worked) and for **comparing pipeline variants**. It isn't trustworthy as an absolute measure of quality. Run it once in Phase 2 on ~30 positive / ~30 negative cases, then whenever the pipeline changes materially.

---

## 6. L5: Prediction calibration

Every experiment records pre-registered probabilistic predictions tied to factors. Examples:

- "P(≥ 3 of 10 interviewees currently pay ≥ $200/mo for this job) = 0.55"
- "P(reply rate ≥ 10% on 40 personalised messages) = 0.35"
- "P(≥ 1 paid pilot from 10 calls) = 0.25"

**Metrics:** Brier score per factor type; calibration curve (once N ≥ 20 predictions); **optimism bias** = mean(predicted − observed). Apply a learned shrinkage to future `p_*` estimates for each factor type.

**Limitations:** experiments are small and differ from one another; calibration is noisy for months. It is still the only layer that ties the engine's judgment to market reality early.

---

## 7. L6: Commercial outcomes

Track per opportunity that reached *Validating*: time to first dollar, MRR at 3/6/12 months, retention, margin, founder hours invested. Per engine: **realised commercial value per founder-hour** (v1.0's ultimate metric), with expected value per founder-hour computed from the Assessor's distributions at decision time, so the gap between expected and realised is visible.

---

## 8. Continuous improvement mechanisms

1. **Source and strategy yield.** For each source/lens: observations → problems → qualified → advanced by founder → validated. Allocate collection and research budget by yield (Thompson sampling over lenses, reward = founder-advanced opportunities; later = validated ones).
2. **Reason-code mining.** Monthly: distribution of kill reasons by source and vertical. If 60% of kills are `incumbent_strong`, the competitor sweep should run *earlier* (it already should) and sources should shift toward less-served segments.
3. **Case-based prompting (no fine-tuning).** Retrieve the founder's most similar past decisions (with reasons) into triage and comparison prompts. This is how the system "learns the founder's taste" cheaply and reversibly.
4. **Prompt/version A/B.** Each task type's prompt is versioned. New versions run in shadow on a sample of inputs, are compared on L1–L3 metrics, and are promoted only if they're not worse.
5. **Retrospectives.** Each killed-by-experiment or validated opportunity gets an automatic retrospective: which factor estimates were furthest off, which evidence misled, which source originated it. The findings feed the shrinkage factors and source priors.
6. **Model upgrades.** On a new model, re-run a fixed evaluation suite (extraction sample, gold-set competitor recall, known-answer probes, a backtest subset) before switching.
