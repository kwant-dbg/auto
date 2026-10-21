package labour

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ONetStats counts imported O*NET rows.
type ONetStats struct {
	Occupations, Tasks, Ratings int
}

// ImportONet upserts O*NET occupations, tasks and task ratings for a release
// (e.g. "29.3") in one transaction.
func ImportONet(ctx context.Context, db *pgxpool.Pool, files *ONetFiles, release string) (ONetStats, error) {
	var st ONetStats

	occ, tasks, ratings := files.Occupations, files.Tasks, files.Ratings
	oCode, err := occ.Col("O*NET-SOC Code")
	if err != nil {
		return st, err
	}
	oTitle, err := occ.Col("Title")
	if err != nil {
		return st, err
	}
	oDesc, _ := occ.Col("Description")

	tCode, err := tasks.Col("O*NET-SOC Code")
	if err != nil {
		return st, err
	}
	tID, err := tasks.Col("Task ID")
	if err != nil {
		return st, err
	}
	tText, err := tasks.Col("Task")
	if err != nil {
		return st, err
	}
	tType, _ := tasks.Col("Task Type")

	rID, err := ratings.Col("Task ID")
	if err != nil {
		return st, err
	}
	rScale, err := ratings.Col("Scale ID")
	if err != nil {
		return st, err
	}
	rCat, _ := ratings.Col("Category")
	rVal, err := ratings.Col("Data Value")
	if err != nil {
		return st, err
	}

	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			CREATE TEMP TABLE _occ (LIKE onet_occupation) ON COMMIT DROP;
			CREATE TEMP TABLE _task (LIKE onet_task) ON COMMIT DROP;
			CREATE TEMP TABLE _rating (LIKE onet_task_rating) ON COMMIT DROP;`); err != nil {
			return err
		}

		occRows := make([][]any, 0, len(occ.Rows))
		for _, r := range occ.Rows {
			desc := ""
			if oDesc >= 0 {
				desc = r[oDesc]
			}
			occRows = append(occRows, []any{r[oCode], SOCFromONet(r[oCode]), r[oTitle], desc, release})
		}
		n, err := tx.CopyFrom(ctx, pgx.Identifier{"_occ"},
			[]string{"onet_soc_code", "soc_code", "title", "description", "release"}, pgx.CopyFromRows(occRows))
		if err != nil {
			return fmt.Errorf("occupations: %w", err)
		}
		st.Occupations = int(n)

		taskRows := make([][]any, 0, len(tasks.Rows))
		for i, r := range tasks.Rows {
			id, err := strconv.ParseInt(strings.TrimSpace(r[tID]), 10, 64)
			if err != nil {
				return fmt.Errorf("tasks row %d: task id %q: %w", i+2, r[tID], err)
			}
			typ := ""
			if tType >= 0 {
				typ = r[tType]
			}
			taskRows = append(taskRows, []any{id, r[tCode], r[tText], typ, release})
		}
		n, err = tx.CopyFrom(ctx, pgx.Identifier{"_task"},
			[]string{"task_id", "onet_soc_code", "task", "task_type", "release"}, pgx.CopyFromRows(taskRows))
		if err != nil {
			return fmt.Errorf("tasks: %w", err)
		}
		st.Tasks = int(n)

		ratingRows := make([][]any, 0, len(ratings.Rows))
		for i, r := range ratings.Rows {
			id, err := strconv.ParseInt(strings.TrimSpace(r[rID]), 10, 64)
			if err != nil {
				return fmt.Errorf("ratings row %d: task id %q: %w", i+2, r[rID], err)
			}
			v, ok := ParseNumber(r[rVal])
			if !ok {
				continue
			}
			cat := ""
			if rCat >= 0 && !strings.EqualFold(strings.TrimSpace(r[rCat]), "n/a") {
				cat = strings.TrimSpace(r[rCat])
			}
			ratingRows = append(ratingRows, []any{id, strings.TrimSpace(r[rScale]), cat, v})
		}
		n, err = tx.CopyFrom(ctx, pgx.Identifier{"_rating"},
			[]string{"task_id", "scale_id", "category", "data_value"}, pgx.CopyFromRows(ratingRows))
		if err != nil {
			return fmt.Errorf("ratings: %w", err)
		}
		st.Ratings = int(n)

		_, err = tx.Exec(ctx, `
			INSERT INTO onet_occupation SELECT DISTINCT ON (onet_soc_code) * FROM _occ
			ON CONFLICT (onet_soc_code) DO UPDATE SET soc_code = EXCLUDED.soc_code, title = EXCLUDED.title,
				description = EXCLUDED.description, release = EXCLUDED.release;
			INSERT INTO onet_task SELECT DISTINCT ON (task_id) * FROM _task
			WHERE onet_soc_code IN (SELECT onet_soc_code FROM onet_occupation)
			ON CONFLICT (task_id) DO UPDATE SET onet_soc_code = EXCLUDED.onet_soc_code, task = EXCLUDED.task,
				task_type = EXCLUDED.task_type, release = EXCLUDED.release;
			INSERT INTO onet_task_rating SELECT DISTINCT ON (task_id, scale_id, category) * FROM _rating
			WHERE task_id IN (SELECT task_id FROM onet_task)
			ON CONFLICT (task_id, scale_id, category) DO UPDATE SET data_value = EXCLUDED.data_value;`)
		return err
	})
	return st, err
}

// OEWSStats counts imported OEWS rows.
type OEWSStats struct {
	Rows, Skipped int
}

// ImportOEWS upserts an OEWS table (national or national-by-industry file)
// for a survey year. Suppressed or top-coded cells are stored as NULL.
func ImportOEWS(ctx context.Context, db *pgxpool.Pool, t *Table, year int) (OEWSStats, error) {
	var st OEWSStats
	cols := map[string]int{}
	for name, aliases := range map[string][]string{
		"area":        {"AREA"},
		"naics":       {"NAICS"},
		"naics_title": {"NAICS_TITLE"},
		"occ_code":    {"OCC_CODE"},
		"occ_title":   {"OCC_TITLE"},
		"o_group":     {"O_GROUP", "OCC_GROUP"},
		"tot_emp":     {"TOT_EMP"},
		"a_median":    {"A_MEDIAN"},
		"a_mean":      {"A_MEAN"},
	} {
		i, err := t.Col(aliases...)
		if err != nil && name != "area" && name != "naics" && name != "naics_title" && name != "o_group" {
			return st, err
		}
		cols[name] = i
	}
	get := func(r []string, name string, def string) string {
		if i := cols[name]; i >= 0 && i < len(r) {
			return strings.TrimSpace(r[i])
		}
		return def
	}
	num := func(r []string, name string) any {
		if v, ok := ParseNumber(get(r, name, "")); ok {
			return v
		}
		return nil
	}

	rows := make([][]any, 0, len(t.Rows))
	for _, r := range t.Rows {
		occ := get(r, "occ_code", "")
		if occ == "" {
			st.Skipped++
			continue
		}
		rows = append(rows, []any{
			year, get(r, "area", "99"), get(r, "naics", "000000"), get(r, "naics_title", "Cross-industry"),
			occ, get(r, "occ_title", ""), get(r, "o_group", ""),
			num(r, "tot_emp"), num(r, "a_median"), num(r, "a_mean"),
		})
	}
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE _oews (LIKE oews_estimate) ON COMMIT DROP`); err != nil {
			return err
		}
		n, err := tx.CopyFrom(ctx, pgx.Identifier{"_oews"},
			[]string{"year", "area", "naics", "naics_title", "occ_code", "occ_title", "o_group", "tot_emp", "a_median", "a_mean"},
			pgx.CopyFromRows(rows))
		if err != nil {
			return err
		}
		st.Rows = int(n)
		_, err = tx.Exec(ctx, `
			INSERT INTO oews_estimate SELECT DISTINCT ON (year, area, naics, occ_code) * FROM _oews
			ON CONFLICT (year, area, naics, occ_code) DO UPDATE SET naics_title = EXCLUDED.naics_title,
				occ_title = EXCLUDED.occ_title, o_group = EXCLUDED.o_group, tot_emp = EXCLUDED.tot_emp,
				a_median = EXCLUDED.a_median, a_mean = EXCLUDED.a_mean`)
		return err
	})
	return st, err
}

// AIUsageStats counts imported AI-usage rows and how many matched an O*NET task.
type AIUsageStats struct {
	Rows, Matched int
}

// ImportAIUsage imports a task-level AI-usage table (e.g. a release of the
// Anthropic Economic Index). Column names differ between releases, so they
// are passed in. Tasks are matched to O*NET by normalised text.
func ImportAIUsage(ctx context.Context, db *pgxpool.Pool, t *Table, release, taskCol, valueCol, metric string) (AIUsageStats, error) {
	var st AIUsageStats
	ti, err := t.Col(taskCol)
	if err != nil {
		return st, err
	}
	vi, err := t.Col(valueCol)
	if err != nil {
		return st, err
	}

	taskIDs := map[string]int64{}
	rows, err := db.Query(ctx, `SELECT task_id, task FROM onet_task`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return st, err
		}
		taskIDs[NormTask(text)] = id
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	agg := map[string]float64{} // sum duplicates within a release
	for _, r := range t.Rows {
		v, ok := ParseNumber(r[vi])
		if !ok || strings.TrimSpace(r[ti]) == "" {
			continue
		}
		agg[strings.TrimSpace(r[ti])] += v
	}
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		for text, v := range agg {
			var id any
			if tid, ok := taskIDs[NormTask(text)]; ok {
				id = tid
				st.Matched++
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO ai_task_usage (release, task_text, task_id, metric, value) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (release, task_text, metric) DO UPDATE SET task_id = EXCLUDED.task_id, value = EXCLUDED.value`,
				release, text, id, metric, v); err != nil {
				return err
			}
			st.Rows++
		}
		return nil
	})
	return st, err
}

// Seed is one ranked (task, industry) hypothesis seed.
type Seed struct {
	TaskID     int64
	Task       string
	Occupation string
	NAICS      string
	NAICSTitle string
	Employment float64
	MedianWage float64
	Share      float64
	EstSpend   float64
	AIUsage    *float64
}

// Seeds ranks tasks by estimated annual spend. Industry-specific rows are used
// when present; crossIndustry selects the all-industry rows instead.
// These are hypothesis seeds for research, not opportunities (docs/02 §3.1).
func Seeds(ctx context.Context, db *pgxpool.Pool, limit int, crossIndustry bool, naicsPrefix string) ([]Seed, error) {
	rows, err := db.Query(ctx, `
		SELECT task_id, task, occupation, naics, naics_title, tot_emp, a_median, share, est_task_spend, ai_usage
		FROM labour_task_spend
		WHERE (CASE WHEN $2 THEN naics = '000000' ELSE naics <> '000000' END)
		  AND naics LIKE $3 || '%'
		ORDER BY est_task_spend DESC
		LIMIT $1`, limit, crossIndustry, naicsPrefix)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Seed, error) {
		var s Seed
		err := r.Scan(&s.TaskID, &s.Task, &s.Occupation, &s.NAICS, &s.NAICSTitle, &s.Employment, &s.MedianWage,
			&s.Share, &s.EstSpend, &s.AIUsage)
		return s, err
	})
}
