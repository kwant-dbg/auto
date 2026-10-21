-- Problem consolidation: merging problems that describe the same underlying
-- problem (PRB-5). Merged problems keep their row, pointing at the survivor.
ALTER TABLE problem ADD COLUMN merged_into bigint REFERENCES problem (id);
ALTER TABLE problem ADD COLUMN consolidated_at timestamptz;

CREATE TABLE problem_merge (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    from_id        bigint NOT NULL REFERENCES problem (id),
    into_id        bigint NOT NULL REFERENCES problem (id),
    similarity     double precision,
    model          text NOT NULL,
    prompt_version text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);
