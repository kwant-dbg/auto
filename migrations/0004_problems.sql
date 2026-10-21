-- Week 3: problem formation, triage, opportunities and questions.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- The normalised statement every comparison uses (docs/02 §5). Stored, so
-- trigram search can index it.
ALTER TABLE observation ADD COLUMN statement text GENERATED ALWAYS AS (
    actor_segment || ' need to ' || job ||
    CASE WHEN obstacle <> '' THEN ' but ' || obstacle ELSE '' END ||
    CASE WHEN consequence <> '' THEN ', causing ' || consequence ELSE '' END
) STORED;

-- An observation belongs to exactly one problem.
ALTER TABLE problem_observation ADD CONSTRAINT problem_observation_one_problem UNIQUE (observation_id);
CREATE INDEX problem_observation_problem_idx ON problem_observation (problem_id);

ALTER TABLE problem
    ADD COLUMN observation_count    integer NOT NULL DEFAULT 0,
    ADD COLUMN best_tier            smallint,
    ADD COLUMN t1_origin_count      integer NOT NULL DEFAULT 0,
    ADD COLUMN triaged_origin_count integer,            -- origins at the last triage
    ADD COLUMN last_triaged_at      timestamptz,
    ADD COLUMN updated_at           timestamptz NOT NULL DEFAULT now();
CREATE INDEX problem_statement_trgm_idx ON problem USING gin (canonical_statement gin_trgm_ops);
CREATE INDEX problem_triage_idx ON problem (independent_origin_count DESC) WHERE status = 'active';

-- One opportunity per problem for now; segments live on the problem.
ALTER TABLE opportunity ADD CONSTRAINT opportunity_one_per_problem UNIQUE (problem_id);

ALTER TABLE question ADD COLUMN p_kill double precision;

-- Every clustering decision, for measuring dedupe precision (PRB-6) and for
-- re-running with better models.
CREATE TABLE cluster_decision (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    observation_id bigint NOT NULL REFERENCES observation (id),
    method         text NOT NULL CHECK (method IN ('auto_attach', 'auto_new', 'adjudicated')),
    similarity     double precision,
    candidate_ids  bigint[] NOT NULL DEFAULT '{}',
    relation       text NOT NULL CHECK (relation IN ('same', 'narrower', 'broader', 'different')),
    problem_id     bigint NOT NULL REFERENCES problem (id),
    model          text,
    prompt_version text,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Trigram candidate search compares a new observation with every member of
-- each problem, not just one representative statement.
CREATE INDEX observation_statement_trgm_idx ON observation USING gin (statement gin_trgm_ops);
