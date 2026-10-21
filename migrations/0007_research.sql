-- Week 4: research, verification, assessment and debate.

ALTER TABLE finding
    ADD COLUMN opportunity_id bigint REFERENCES opportunity (id),
    ADD COLUMN task_id        bigint REFERENCES task (id),
    ADD COLUMN stance         text NOT NULL DEFAULT 'supports' CHECK (stance IN ('supports', 'contradicts', 'neutral')),
    ADD COLUMN bears_on       text NOT NULL DEFAULT '',     -- gate or factor code
    ADD COLUMN verifier_note  text NOT NULL DEFAULT '';
CREATE INDEX finding_opportunity_idx ON finding (opportunity_id);

ALTER TABLE question
    ADD COLUMN answer      text,
    ADD COLUMN answered_at timestamptz;

ALTER TABLE opportunity
    ADD COLUMN no_change_streak integer NOT NULL DEFAULT 0,  -- research rounds without a material change
    ADD COLUMN last_research_at timestamptz;

ALTER TABLE assessment
    ADD COLUMN inputs           jsonb NOT NULL DEFAULT '{}',  -- the model's raw factor output
    ADD COLUMN next_experiments jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN p_build          double precision;

ALTER TABLE debate_argument
    ADD COLUMN verdict text CHECK (verdict IN ('supported', 'partial', 'unsupported'));

ALTER TABLE debate
    ADD COLUMN summary text NOT NULL DEFAULT '',
    ADD COLUMN judged_at timestamptz;
