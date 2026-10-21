-- Labour-market decomposition (docs/02 §3.1): wage pool per (task, industry).
--
-- A task's share of an occupation's wage pool is its O*NET importance rating
-- divided by the summed importance of every task mapped to the same SOC code.
-- This is a ranking heuristic, not an accounting identity.

CREATE VIEW labour_task_share AS
SELECT t.task_id,
       t.task,
       o.soc_code,
       o.title AS occupation,
       r.data_value AS importance,
       r.data_value / sum(r.data_value) OVER (PARTITION BY o.soc_code) AS share
FROM onet_task t
JOIN onet_occupation o USING (onet_soc_code)
JOIN onet_task_rating r ON r.task_id = t.task_id AND r.scale_id = 'IM' AND r.category = '';

CREATE VIEW labour_task_spend AS
SELECT s.task_id,
       s.task,
       s.soc_code,
       s.occupation,
       s.share,
       e.year,
       e.naics,
       e.naics_title,
       e.tot_emp,
       e.a_median,
       e.tot_emp * e.a_median           AS wage_pool,
       e.tot_emp * e.a_median * s.share AS est_task_spend,
       u.ai_usage
FROM labour_task_share s
JOIN oews_estimate e
  ON e.occ_code = s.soc_code
 AND e.area = '99'
 AND e.year = (SELECT max(year) FROM oews_estimate)
LEFT JOIN (
    SELECT task_id, sum(value) AS ai_usage
    FROM ai_task_usage
    WHERE task_id IS NOT NULL
      AND release = (SELECT max(release) FROM ai_task_usage)
    GROUP BY task_id
) u ON u.task_id = s.task_id
WHERE e.tot_emp IS NOT NULL
  AND e.a_median IS NOT NULL;
