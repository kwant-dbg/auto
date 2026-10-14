# Autonomous Opportunity Engine (AOE): Design

Pre-implementation strategy and architecture for a personal system that finds, evaluates, and validates commercially viable software opportunities for a solo technical founder.

**Status:** design review complete. Ready for Phase 0 (one-week manual run), then Phase 1 build. No implementation code yet.

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
