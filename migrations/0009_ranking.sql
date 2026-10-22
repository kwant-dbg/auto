-- Pairwise ranking (RPT-2): Bradley-Terry strength from the latest ranking
-- run over the opportunities eligible for cards.
ALTER TABLE opportunity
    ADD COLUMN rank_score double precision,
    ADD COLUMN ranked_at  timestamptz;
CREATE TABLE ranking_run (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id        bigint REFERENCES task (id),
    opportunity_ids bigint[] NOT NULL,
    judgments      jsonb NOT NULL,
    order_agreement double precision,  -- share of pairs judged the same in both orders
    model          text NOT NULL,
    prompt_version text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);
