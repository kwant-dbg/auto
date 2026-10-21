-- Week 2: extraction tracking, time-series metrics, knowledge-base search.

-- Numeric facts about a subject over time (e.g. a marketplace app's installs).
-- Kept out of documents so a changing counter doesn't create a new document
-- version, and a new extraction, every day.
CREATE TABLE metric_observation (
    source_id   bigint NOT NULL REFERENCES source (id),
    subject     text NOT NULL,                -- e.g. atlassian:com.example.app
    name        text NOT NULL,                -- e.g. installs, review_count, avg_stars, price_usd_10_users
    value       double precision NOT NULL,
    observed_on date NOT NULL DEFAULT current_date,
    observed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source_id, subject, name, observed_on)
);

-- One row per (document, extractor prompt version). A row is written when the
-- document is queued, so the scheduler never batches it twice.
CREATE TABLE extraction (
    raw_document_id   bigint NOT NULL REFERENCES raw_document (id),
    prompt_version    text NOT NULL,
    status            text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'done', 'failed')),
    task_id           bigint REFERENCES task (id),
    attempts          integer NOT NULL DEFAULT 1,
    observation_count integer NOT NULL DEFAULT 0,
    rejected_count    integer NOT NULL DEFAULT 0,  -- quotes that failed the span check
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (raw_document_id, prompt_version)
);
CREATE INDEX extraction_task_idx ON extraction (task_id);

-- Extracted quotes that could not be found verbatim in their snapshot. Kept
-- for monitoring the extractor (L1 metrics, docs/07 §2), never shown as evidence.
CREATE TABLE rejected_extraction (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    raw_document_id bigint NOT NULL REFERENCES raw_document (id),
    quote           text NOT NULL,
    reason          text NOT NULL,
    prompt_version  text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE observation ADD COLUMN injection_suspect boolean NOT NULL DEFAULT false;

-- Knowledge-base search (kb_search tool).
CREATE INDEX observation_fts_idx ON observation
    USING gin (to_tsvector('english', quote || ' ' || actor_segment || ' ' || job || ' ' || obstacle));

-- The embedding sweeper looks for observations without vectors.
CREATE INDEX observation_unembedded_idx ON observation (id) WHERE embedding IS NULL;
