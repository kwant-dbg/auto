-- AOE initial schema (PRD §8).
-- Conventions: bigint identity ids, timestamptz everywhere, append-only where noted.
-- LLM-produced rows carry model, prompt_version, input_hash.

CREATE EXTENSION IF NOT EXISTS vector;

-- ---------------------------------------------------------------------------
-- Control plane
-- ---------------------------------------------------------------------------

CREATE TABLE control (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO control (key, value) VALUES ('paused', 'false');

-- ---------------------------------------------------------------------------
-- Sources and raw documents
-- ---------------------------------------------------------------------------

CREATE TABLE tos_entry (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_type        text NOT NULL UNIQUE,
    access_method      text NOT NULL,          -- official_api | bulk_download | public_feed | manual_sample
    terms_url          text NOT NULL,
    reviewed_at        date NOT NULL,
    allowed_use        text NOT NULL,
    rate_limit_per_min integer NOT NULL CHECK (rate_limit_per_min > 0),
    notes              text NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE source (
    id                      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                    text NOT NULL UNIQUE,
    type                    text NOT NULL,
    config                  jsonb NOT NULL DEFAULT '{}',
    cadence                 interval NOT NULL DEFAULT '1 day',
    backoff_factor          integer NOT NULL DEFAULT 1 CHECK (backoff_factor >= 1),
    cursor                  jsonb,
    tos_entry_id            bigint REFERENCES tos_entry (id),
    enabled                 boolean NOT NULL DEFAULT false,
    next_run_at             timestamptz NOT NULL DEFAULT now(),
    last_run_at             timestamptz,
    last_success_at         timestamptz,
    last_new_document_at    timestamptz,
    consecutive_empty_runs  integer NOT NULL DEFAULT 0,
    consecutive_errors      integer NOT NULL DEFAULT 0,
    last_error              text,
    created_at              timestamptz NOT NULL DEFAULT now(),
    -- SRC-2: a connector cannot be enabled without a ToS register entry.
    CONSTRAINT source_enabled_requires_tos CHECK (NOT enabled OR tos_entry_id IS NOT NULL)
);

CREATE TABLE raw_document (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id         bigint NOT NULL REFERENCES source (id),
    canonical_url     text NOT NULL,
    external_id       text,
    content_hash      text NOT NULL,           -- sha256 hex of the stored bytes
    blob_path         text NOT NULL,           -- content-addressed path relative to the blob root
    mime              text NOT NULL,
    byte_size         integer NOT NULL,
    published_at      timestamptz,
    fetched_at        timestamptz NOT NULL DEFAULT now(),
    prev_version_id   bigint REFERENCES raw_document (id),
    author_hash       text,
    org_hint          text,
    injection_suspect boolean NOT NULL DEFAULT false,
    UNIQUE (source_id, canonical_url, content_hash)
);
CREATE INDEX raw_document_url_idx ON raw_document (source_id, canonical_url, fetched_at DESC);

-- ---------------------------------------------------------------------------
-- Labour-market reference data (discovery lens 1, docs/02 §3.1)
-- ---------------------------------------------------------------------------

CREATE TABLE onet_occupation (
    onet_soc_code text PRIMARY KEY,            -- e.g. 15-1252.00
    soc_code      text NOT NULL,               -- e.g. 15-1252 (joins to OEWS)
    title         text NOT NULL,
    description   text NOT NULL DEFAULT '',
    release       text NOT NULL
);
CREATE INDEX onet_occupation_soc_idx ON onet_occupation (soc_code);

CREATE TABLE onet_task (
    task_id       bigint PRIMARY KEY,
    onet_soc_code text NOT NULL REFERENCES onet_occupation (onet_soc_code),
    task          text NOT NULL,
    task_type     text NOT NULL DEFAULT '',    -- Core | Supplemental
    release       text NOT NULL
);
CREATE INDEX onet_task_soc_idx ON onet_task (onet_soc_code);

CREATE TABLE onet_task_rating (
    task_id    bigint NOT NULL REFERENCES onet_task (task_id),
    scale_id   text NOT NULL,                  -- IM (importance), RT (relevance), FT (frequency)
    category   text NOT NULL DEFAULT '',       -- populated for FT
    data_value double precision NOT NULL,
    PRIMARY KEY (task_id, scale_id, category)
);

CREATE TABLE oews_estimate (
    year        integer NOT NULL,
    area        text NOT NULL,                 -- 99 = U.S.
    naics       text NOT NULL,                 -- 000000 = cross-industry
    naics_title text NOT NULL,
    occ_code    text NOT NULL,                 -- SOC, e.g. 15-1252
    occ_title   text NOT NULL,
    o_group     text NOT NULL DEFAULT '',      -- detailed | broad | major | total
    tot_emp     double precision,
    a_median    double precision,
    a_mean      double precision,
    PRIMARY KEY (year, area, naics, occ_code)
);

CREATE TABLE ai_task_usage (
    release   text NOT NULL,
    task_text text NOT NULL,                   -- as published; matched to onet_task.task
    task_id   bigint REFERENCES onet_task (task_id),
    metric    text NOT NULL,                   -- e.g. pct_conversations
    value     double precision NOT NULL,
    PRIMARY KEY (release, task_text, metric)
);

-- ---------------------------------------------------------------------------
-- Evidence: observations, problems
-- ---------------------------------------------------------------------------

CREATE TABLE observation (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    raw_document_id  bigint NOT NULL REFERENCES raw_document (id),
    span_start       integer NOT NULL CHECK (span_start >= 0),
    span_end         integer NOT NULL CHECK (span_end > span_start),
    quote            text NOT NULL,
    category         text NOT NULL,            -- spend | complaint | offer | job_task | change | other
    evidence_tier    smallint NOT NULL CHECK (evidence_tier BETWEEN 0 AND 7),
    actor_segment    text NOT NULL,
    job              text NOT NULL,
    obstacle         text NOT NULL DEFAULT '',
    consequence      text NOT NULL DEFAULT '',
    specificity      smallint NOT NULL DEFAULT 0 CHECK (specificity BETWEEN 0 AND 5),
    spend_hints      jsonb NOT NULL DEFAULT '{}',
    origin_key       text NOT NULL,            -- independence counting
    embedding        vector,
    embedding_model  text,
    verified         boolean NOT NULL DEFAULT false,
    model            text NOT NULL,
    prompt_version   text NOT NULL,
    input_hash       text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX observation_doc_idx ON observation (raw_document_id);

CREATE TABLE problem (
    id                       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    parent_id                bigint REFERENCES problem (id),
    canonical_statement      text NOT NULL,
    segment                  text NOT NULL,
    industry_naics           text,
    embedding                vector,
    embedding_model          text,
    independent_origin_count integer NOT NULL DEFAULT 0,
    status                   text NOT NULL DEFAULT 'active',
    created_at               timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE problem_observation (
    problem_id     bigint NOT NULL REFERENCES problem (id),
    observation_id bigint NOT NULL REFERENCES observation (id),
    attach_method  text NOT NULL CHECK (attach_method IN ('auto', 'adjudicated', 'recluster', 'human')),
    score          double precision,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (problem_id, observation_id)
);

-- ---------------------------------------------------------------------------
-- Opportunities, gates, factors, questions, findings
-- ---------------------------------------------------------------------------

CREATE TABLE opportunity (
    id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    problem_id            bigint NOT NULL REFERENCES problem (id),
    segment               text NOT NULL,
    state                 text NOT NULL DEFAULT 'candidate'
        CHECK (state IN ('candidate', 'qualified', 'dossier', 'validating', 'building', 'live', 'parked', 'killed')),
    stage_entered_at      timestamptz NOT NULL DEFAULT now(),
    budget_units_total    integer NOT NULL DEFAULT 1,
    budget_units_used     integer NOT NULL DEFAULT 0,
    saturated             boolean NOT NULL DEFAULT false,
    current_assessment_id bigint,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE gate_status (
    opportunity_id bigint NOT NULL REFERENCES opportunity (id),
    gate_code      text NOT NULL,              -- F1..F4, M1..M6
    status         text NOT NULL CHECK (status IN ('PASS', 'FAIL', 'UNKNOWN', 'SKIPPED')),
    evidence_ids   bigint[] NOT NULL DEFAULT '{}',
    rationale      text NOT NULL DEFAULT '',
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (opportunity_id, gate_code)
);

CREATE TABLE assessment (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    opportunity_id    bigint NOT NULL REFERENCES opportunity (id),
    ev_p10            double precision,
    ev_p50            double precision,
    ev_p90            double precision,
    p_ge_1k           double precision,
    p_ge_5k           double precision,
    weakest_factor    text,
    paths             jsonb NOT NULL DEFAULT '[]',
    first10_plan      jsonb NOT NULL DEFAULT '{}',
    debate_summary    text NOT NULL DEFAULT '',
    model             text NOT NULL,
    prompt_version    text NOT NULL,
    input_hash        text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE opportunity
    ADD CONSTRAINT opportunity_current_assessment_fk FOREIGN KEY (current_assessment_id) REFERENCES assessment (id);

CREATE TABLE factor_estimate (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    assessment_id     bigint NOT NULL REFERENCES assessment (id),
    factor_code       text NOT NULL,
    low               double precision NOT NULL,
    base              double precision NOT NULL,
    high              double precision NOT NULL,
    unit              text NOT NULL,
    evidence_tier_max smallint CHECK (evidence_tier_max BETWEEN 0 AND 7),
    citation_ids      bigint[] NOT NULL DEFAULT '{}',
    clamped_to_prior  boolean NOT NULL DEFAULT false,
    CHECK (low <= base AND base <= high),
    UNIQUE (assessment_id, factor_code)
);

CREATE TABLE prospect (
    id                       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    opportunity_id           bigint NOT NULL REFERENCES opportunity (id),
    org_name                 text NOT NULL,
    segment_fit_evidence_id  bigint REFERENCES observation (id),
    contact_path             text NOT NULL,
    source_url               text NOT NULL,
    status                   text NOT NULL DEFAULT 'new',
    created_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (opportunity_id, org_name)
);

CREATE TABLE question (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    opportunity_id     bigint NOT NULL REFERENCES opportunity (id),
    text               text NOT NULL,
    linked_code        text NOT NULL,          -- factor or gate code
    kind               text NOT NULL DEFAULT 'standard' CHECK (kind IN ('kill', 'standard')),
    status             text NOT NULL DEFAULT 'open'
        CHECK (status IN ('open', 'answered', 'saturated', 'needs_human')),
    voe_estimate       double precision,
    cost_estimate      double precision,
    parent_question_id bigint REFERENCES question (id),
    depth              smallint NOT NULL DEFAULT 0 CHECK (depth BETWEEN 0 AND 2),
    origin             text NOT NULL CHECK (origin IN ('triage', 'agent', 'human', 'debate')),
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE finding (
    id                      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    question_id             bigint NOT NULL REFERENCES question (id),
    claim                   text NOT NULL,
    citation_observation_ids bigint[] NOT NULL DEFAULT '{}',
    citation_spans          jsonb NOT NULL DEFAULT '[]',  -- [{raw_document_id, start, end, quote}]
    verdict                 text CHECK (verdict IN ('supported', 'partial', 'unsupported')),
    retracted               boolean NOT NULL DEFAULT false,
    retraction_reason       text,
    model                   text NOT NULL,
    prompt_version          text NOT NULL,
    input_hash              text NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Debate (docs/04 §4.1)
-- ---------------------------------------------------------------------------

CREATE TABLE debate (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    opportunity_id bigint NOT NULL REFERENCES opportunity (id),
    assessment_id  bigint REFERENCES assessment (id),
    status         text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'judged', 'failed')),
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE debate_argument (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    debate_id     bigint NOT NULL REFERENCES debate (id),
    side          text NOT NULL CHECK (side IN ('bull', 'bear')),
    round         smallint NOT NULL CHECK (round IN (1, 2)),
    text          text NOT NULL,
    citation_spans jsonb NOT NULL DEFAULT '[]',
    struck        boolean NOT NULL DEFAULT false,
    strike_reason text,
    model         text NOT NULL,
    prompt_version text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE debate_ruling (
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    debate_id bigint NOT NULL REFERENCES debate (id),
    claim     text NOT NULL,
    verdict   text NOT NULL CHECK (verdict IN ('bull_holds', 'bear_holds', 'unresolved')),
    rationale text NOT NULL,
    model     text NOT NULL,
    prompt_version text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE objection (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    opportunity_id bigint NOT NULL REFERENCES opportunity (id),
    debate_id      bigint REFERENCES debate (id),
    text           text NOT NULL,
    evidence_ids   bigint[] NOT NULL DEFAULT '{}',
    severity       smallint NOT NULL DEFAULT 2 CHECK (severity BETWEEN 1 AND 3),
    later_outcome  text CHECK (later_outcome IN ('proved_right', 'proved_wrong', 'unknown')),
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Validation (Phase 3; created now so the model is complete)
-- ---------------------------------------------------------------------------

CREATE TABLE experiment (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    opportunity_id    bigint NOT NULL REFERENCES opportunity (id),
    hypothesis        text NOT NULL,
    factor_codes      text[] NOT NULL DEFAULT '{}',
    method            text NOT NULL,
    sample            text NOT NULL DEFAULT '',
    success_threshold text NOT NULL,
    kill_threshold    text NOT NULL,
    budget            jsonb NOT NULL DEFAULT '{}',
    results           jsonb,
    brier             double precision,
    conclusion        text,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE prediction (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    experiment_id bigint NOT NULL REFERENCES experiment (id),
    factor_code   text NOT NULL,
    statement     text NOT NULL,
    probability   double precision NOT NULL CHECK (probability BETWEEN 0 AND 1),
    outcome       boolean,
    scored_at     timestamptz
);

CREATE TABLE action (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    experiment_id   bigint REFERENCES experiment (id),
    type            text NOT NULL,
    payload         jsonb NOT NULL,
    idempotency_key text NOT NULL UNIQUE,
    status          text NOT NULL DEFAULT 'proposed'
        CHECK (status IN ('proposed', 'approved', 'denied', 'executed', 'failed')),
    approved_by     text,
    executed_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Decisions and feedback (append-only)
-- ---------------------------------------------------------------------------

CREATE TABLE decision (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    subject_type    text NOT NULL,
    subject_id      bigint NOT NULL,
    decision_type   text NOT NULL,
    from_state      text,
    to_state        text,
    actor           text NOT NULL CHECK (actor IN ('human', 'system')),
    reason_code     text,
    notes           text NOT NULL DEFAULT '',
    evidence_ids    bigint[] NOT NULL DEFAULT '{}',
    factor_snapshot jsonb,
    wake_conditions jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX decision_subject_idx ON decision (subject_type, subject_id, created_at DESC);

CREATE TABLE feedback (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    subject_type text NOT NULL,
    subject_id   bigint NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('rating', 'pairwise', 'grade')),
    value        text NOT NULL,
    reason_code  text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Work queue and model-call ledger
-- ---------------------------------------------------------------------------

CREATE TABLE task (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type           text NOT NULL,
    opportunity_id bigint REFERENCES opportunity (id),
    question_id    bigint REFERENCES question (id),
    dedupe_key     text,
    priority       integer NOT NULL DEFAULT 0,
    state          text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done', 'dead')),
    run_after      timestamptz NOT NULL DEFAULT now(),
    lease_until    timestamptz,
    attempts       integer NOT NULL DEFAULT 0,
    max_attempts   integer NOT NULL DEFAULT 3 CHECK (max_attempts >= 1),
    inputs         jsonb NOT NULL DEFAULT '{}',
    outputs        jsonb,
    last_error     text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    started_at     timestamptz,
    finished_at    timestamptz
);
-- SCH-2: equivalent work is admitted once.
CREATE UNIQUE INDEX task_dedupe_idx ON task (dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX task_claim_idx ON task (priority DESC, id) WHERE state IN ('queued', 'running');

CREATE TABLE llm_call (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id             bigint REFERENCES task (id),
    task_type           text NOT NULL,
    model               text NOT NULL,
    prompt_version      text NOT NULL,
    claude_code_version text NOT NULL DEFAULT '',
    session_id          text,
    num_turns           integer,
    est_cost_usd        double precision,  -- CLI estimate, not a bill
    usage               jsonb,
    latency_ms          integer NOT NULL,
    exit_code           integer NOT NULL,
    status              text NOT NULL CHECK (status IN ('ok', 'error', 'timeout', 'invalid_output')),
    error               text,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX llm_call_day_idx ON llm_call (created_at);
