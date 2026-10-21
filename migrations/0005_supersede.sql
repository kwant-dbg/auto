-- Re-extraction with a newer prompt or model supersedes a document's older
-- observations instead of duplicating them (PRD: "allow reanalysis using
-- newer AI models").
ALTER TABLE observation ADD COLUMN superseded boolean NOT NULL DEFAULT false;
CREATE INDEX observation_current_idx ON observation (raw_document_id) WHERE NOT superseded;
