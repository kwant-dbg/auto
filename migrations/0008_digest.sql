-- Week 5: the weekly digest, baseline B0 cards, blind pairs and feedback.

-- One digest per ISO week (RPT-1). The markdown is stored exactly as sent;
-- stats hold the digest's own measurements (e.g. quotes span-checked).
CREATE TABLE digest (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    week           text NOT NULL UNIQUE,              -- ISO week, e.g. 2026-W43
    period_start   timestamptz NOT NULL,
    period_end     timestamptz NOT NULL,
    markdown       text NOT NULL,
    generated_by   text NOT NULL CHECK (generated_by IN ('system', 'human')),
    stats          jsonb NOT NULL DEFAULT '{}',
    review_minutes integer CHECK (review_minutes >= 0),  -- founder time (A8)
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- A one-shot deep-research run with the founder profile (B0, docs/07 §4).
CREATE TABLE baseline_run (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    week           text NOT NULL,
    task_id        bigint REFERENCES task (id),
    model          text NOT NULL,
    prompt_version text NOT NULL,
    num_turns      integer,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Cards B0 produced, in the same fields as AOE's cards.
CREATE TABLE baseline_card (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id     bigint NOT NULL REFERENCES baseline_run (id),
    card       jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- A blind pair shown in a digest: one AOE card against one B0 card, with
-- the AOE card's side drawn at random (RPT-3).
CREATE TABLE card_pair (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    digest_id          bigint NOT NULL REFERENCES digest (id),
    position           smallint NOT NULL,
    opportunity_id     bigint NOT NULL REFERENCES opportunity (id),
    baseline_card_id   bigint NOT NULL REFERENCES baseline_card (id),
    aoe_side           char(1) NOT NULL CHECK (aoe_side IN ('A', 'B')),
    UNIQUE (digest_id, position)
);

-- What each digest showed, so feedback, duplicate checks and audits refer
-- to exactly what the founder saw.
CREATE TABLE digest_item (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    digest_id      bigint NOT NULL REFERENCES digest (id),
    kind           text NOT NULL CHECK (kind IN ('card', 'approval', 'spot_check')),
    position       smallint NOT NULL,
    opportunity_id bigint REFERENCES opportunity (id),
    finding_id     bigint REFERENCES finding (id),
    question_id    bigint REFERENCES question (id),
    snapshot       jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX digest_item_opportunity_idx ON digest_item (opportunity_id) WHERE kind = 'card';

-- Novelty ("would I have thought of this myself?", 1-5) is its own kind.
ALTER TABLE feedback DROP CONSTRAINT feedback_kind_check;
ALTER TABLE feedback ADD CONSTRAINT feedback_kind_check CHECK (kind IN ('rating', 'pairwise', 'grade', 'novelty'));
CREATE INDEX feedback_subject_idx ON feedback (subject_type, subject_id, kind, created_at DESC);
