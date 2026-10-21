package labour_test

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kwant-dbg/auto/internal/labour"
	"github.com/kwant-dbg/auto/internal/testdb"
)

// Minimal O*NET text-database fixture: two occupations, four tasks.
var onetFixture = map[string]string{
	"Occupation Data.txt": "O*NET-SOC Code\tTitle\tDescription\n" +
		"43-3031.00\tBookkeeping, Accounting, and Auditing Clerks\tCompute, classify, and record numerical data.\n" +
		"43-4051.00\tCustomer Service Representatives\tInteract with customers.\n",
	"Task Statements.txt": "O*NET-SOC Code\tTask ID\tTask\tTask Type\tIncumbents Responding\tDate\tDomain Source\n" +
		"43-3031.00\t1\tReconcile or note and report discrepancies found in records.\tCore\t50\t08/2023\tIncumbent\n" +
		"43-3031.00\t2\tOperate computers programmed with accounting software to record data.\tCore\t50\t08/2023\tIncumbent\n" +
		"43-4051.00\t3\tResolve customers' service or billing complaints.\tCore\t50\t08/2023\tIncumbent\n" +
		"43-4051.00\t4\tKeep records of customer interactions.\tSupplemental\t50\t08/2023\tIncumbent\n",
	"Task Ratings.txt": "O*NET-SOC Code\tTask ID\tScale ID\tCategory\tData Value\tN\n" +
		"43-3031.00\t1\tIM\tn/a\t4.50\t50\n" +
		"43-3031.00\t2\tIM\tn/a\t3.50\t50\n" +
		"43-3031.00\t1\tFT\t5\t30.0\t50\n" +
		"43-4051.00\t3\tIM\tn/a\t4.00\t50\n" +
		"43-4051.00\t4\tIM\tn/a\t4.00\t50\n",
}

const oewsCSV = `AREA,AREA_TITLE,NAICS,NAICS_TITLE,OCC_CODE,OCC_TITLE,O_GROUP,TOT_EMP,A_MEAN,A_MEDIAN
99,U.S.,000000,Cross-industry,43-3031,Bookkeeping clerks,detailed,"1,500,000","49,000","47,000"
99,U.S.,541200,Accounting Services,43-3031,Bookkeeping clerks,detailed,"200,000","50,000","48,000"
99,U.S.,541200,Accounting Services,43-4051,Customer service reps,detailed,"10,000",**,#
99,U.S.,621200,Offices of Dentists,43-4051,Customer service reps,detailed,"50,000","40,000","39,000"
`

func writeFixtures(t *testing.T) (dir, zipPath string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "db_29_3_text")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	zipPath = filepath.Join(t.TempDir(), "onet.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for name, body := range onetFixture {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		w, _ := zw.Create("db_29_3_text/" + name)
		w.Write([]byte(body)) //nolint:errcheck
	}
	zw.Close()
	zf.Close()
	return dir, zipPath
}

func TestLoadONetFromDirAndZip(t *testing.T) {
	dir, zipPath := writeFixtures(t)
	for _, p := range []string{dir, zipPath} {
		f, err := labour.LoadONet(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if len(f.Occupations.Rows) != 2 || len(f.Tasks.Rows) != 4 || len(f.Ratings.Rows) != 5 {
			t.Fatalf("%s: rows = %d/%d/%d", p, len(f.Occupations.Rows), len(f.Tasks.Rows), len(f.Ratings.Rows))
		}
	}
	if _, err := labour.LoadONet(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing files: %v", err)
	}
}

func TestParseHelpers(t *testing.T) {
	if got := labour.SOCFromONet("15-1252.01"); got != "15-1252" {
		t.Errorf("SOCFromONet = %s", got)
	}
	for in, want := range map[string]bool{"1,234": true, "*": false, "**": false, "#": false, "": false, "12.5": true} {
		if _, ok := labour.ParseNumber(in); ok != want {
			t.Errorf("ParseNumber(%q) ok = %v", in, ok)
		}
	}
	if labour.NormTask("  Resolve   complaints. ") != "resolve complaints" {
		t.Errorf("NormTask = %q", labour.NormTask("  Resolve   complaints. "))
	}
	tbl, err := labour.ParseCSV(strings.NewReader("\ufeffTask,Pct\na,1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tbl.Col("task"); err != nil {
		t.Errorf("BOM-prefixed header not matched: %v", err)
	}
}

func TestImportAndSeeds(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	dir, _ := writeFixtures(t)

	files, err := labour.LoadONet(dir)
	if err != nil {
		t.Fatal(err)
	}
	os1, err := labour.ImportONet(ctx, st.DB, files, "29.3")
	if err != nil {
		t.Fatal(err)
	}
	if os1.Occupations != 2 || os1.Tasks != 4 || os1.Ratings != 5 {
		t.Fatalf("onet stats = %+v", os1)
	}
	// Re-import is an upsert, not a duplicate.
	if _, err := labour.ImportONet(ctx, st.DB, files, "29.3"); err != nil {
		t.Fatalf("re-import: %v", err)
	}

	oewsPath := filepath.Join(t.TempDir(), "oews.csv")
	os.WriteFile(oewsPath, []byte(oewsCSV), 0o644) //nolint:errcheck
	tbl, err := labour.OpenTable(oewsPath)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := labour.ImportOEWS(ctx, st.DB, tbl, 2024); err != nil || s.Rows != 4 {
		t.Fatalf("oews = %+v err = %v", s, err)
	}

	usage := "task_name,pct\nReconcile or note and report discrepancies found in records,0.5\nSomething unmatched,0.1\n"
	usagePath := filepath.Join(t.TempDir(), "usage.csv")
	os.WriteFile(usagePath, []byte(usage), 0o644) //nolint:errcheck
	ut, _ := labour.OpenTable(usagePath)
	us, err := labour.ImportAIUsage(ctx, st.DB, ut, "2026-01", "task_name", "pct", "pct_conversations")
	if err != nil || us.Rows != 2 || us.Matched != 1 {
		t.Fatalf("ai usage = %+v err = %v", us, err)
	}

	seeds, err := labour.Seeds(ctx, st.DB, 10, false, "")
	if err != nil {
		t.Fatal(err)
	}
	// Industry rows with a median wage: bookkeeping in 541200 (2 tasks) and
	// customer service in 621200 (2 tasks). The 541200 customer-service row
	// has a top-coded median (#) and is excluded.
	if len(seeds) != 4 {
		t.Fatalf("got %d seeds: %+v", len(seeds), seeds)
	}
	top := seeds[0]
	// 200,000 × 48,000 × 4.5/8 = 5.4e9
	if top.TaskID != 1 || top.NAICS != "541200" || int(top.EstSpend) != 5_400_000_000 {
		t.Fatalf("top seed = %+v", top)
	}
	if top.AIUsage == nil || *top.AIUsage != 0.5 {
		t.Fatalf("AI usage not joined: %+v", top.AIUsage)
	}
	dentists, _ := labour.Seeds(ctx, st.DB, 10, false, "6212")
	if len(dentists) != 2 || dentists[0].NAICS != "621200" {
		t.Fatalf("naics filter: %+v", dentists)
	}
	cross, _ := labour.Seeds(ctx, st.DB, 10, true, "")
	if len(cross) != 2 || cross[0].NAICS != "000000" {
		t.Fatalf("cross-industry: %+v", cross)
	}
}
