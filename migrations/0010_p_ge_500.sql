-- The digest's card bar is P(MRR >= $500 at 12 months) (founder decision,
-- week 5). Existing assessments are backfilled by re-running their seeded
-- simulation over the stored factor ranges (assess.BackfillPGe500).
ALTER TABLE assessment ADD COLUMN p_ge_500 double precision;
