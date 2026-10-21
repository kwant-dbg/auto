// Package labour imports the labour-market reference data behind discovery
// lens 1 (docs/02 §3.1): O*NET occupations and tasks, BLS OEWS employment and
// wages, and task-level AI-usage indices. Joined, they estimate how much is
// spent on each task in each industry.
package labour

import (
	"archive/zip"
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// Table is a parsed delimited file: a header and rows of the same width.
type Table struct {
	Header []string
	Rows   [][]string
	index  map[string]int
}

func newTable(header []string, rows [][]string) *Table {
	t := &Table{Header: header, Rows: rows, index: map[string]int{}}
	for i, h := range header {
		t.index[normHeader(h)] = i
	}
	return t
}

func normHeader(h string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
}

// Col returns the index of the first header matching any name (case-insensitive).
func (t *Table) Col(names ...string) (int, error) {
	for _, n := range names {
		if i, ok := t.index[normHeader(n)]; ok {
			return i, nil
		}
	}
	return -1, fmt.Errorf("column %q not found (have %v)", names[0], t.Header)
}

// ParseTSV parses a tab-delimited file with a header row (the O*NET text format).
func ParseTSV(r io.Reader) (*Table, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var header []string
	var rows [][]string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if header == nil {
			header = fields
			continue
		}
		for len(fields) < len(header) {
			fields = append(fields, "")
		}
		rows = append(rows, fields)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if header == nil {
		return nil, errors.New("empty file")
	}
	return newTable(header, rows), nil
}

// ParseCSV parses a comma-separated file with a header row.
func ParseCSV(r io.Reader) (*Table, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	all, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, errors.New("empty file")
	}
	return newTable(all[0], all[1:]), nil
}

// ParseXLSX parses the first sheet of a workbook (the OEWS download format).
func ParseXLSX(path string) (*Table, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, errors.New("workbook has no sheets")
	}
	rows, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("empty sheet")
	}
	header := rows[0]
	body := rows[1:]
	for i := range body {
		for len(body[i]) < len(header) {
			body[i] = append(body[i], "")
		}
	}
	return newTable(header, body), nil
}

// OpenTable parses a CSV, TSV/TXT or XLSX file by extension.
func OpenTable(path string) (*Table, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".xlsx":
		return ParseXLSX(path)
	case ".csv":
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return ParseCSV(f)
	default:
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return ParseTSV(f)
	}
}

// ONetFiles locates the O*NET text-database files we need inside a directory
// or a downloaded .zip, matching by base name.
type ONetFiles struct {
	Occupations, Tasks, Ratings *Table
}

var onetNames = map[string]string{
	"occupation data.txt": "occ",
	"task statements.txt": "tasks",
	"task ratings.txt":    "ratings",
}

// LoadONet reads the O*NET occupation, task and task-rating files.
func LoadONet(path string) (*ONetFiles, error) {
	found := map[string]*Table{}
	visit := func(name string, open func() (io.ReadCloser, error)) error {
		key, ok := onetNames[strings.ToLower(filepath.Base(name))]
		if !ok || found[key] != nil {
			return nil
		}
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		t, err := ParseTSV(rc)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		found[key] = t
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		err = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			return visit(p, func() (io.ReadCloser, error) { return os.Open(p) })
		})
	} else {
		var zr *zip.ReadCloser
		zr, err = zip.OpenReader(path)
		if err != nil {
			return nil, fmt.Errorf("%s: expected an O*NET text directory or .zip: %w", path, err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			if err = visit(f.Name, f.Open); err != nil {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	for file, key := range onetNames {
		if found[key] == nil {
			return nil, fmt.Errorf("O*NET file %q not found under %s", file, path)
		}
	}
	return &ONetFiles{Occupations: found["occ"], Tasks: found["tasks"], Ratings: found["ratings"]}, nil
}

// SOCFromONet converts an O*NET-SOC code (15-1252.00) to its SOC code (15-1252).
func SOCFromONet(code string) string {
	code = strings.TrimSpace(code)
	if i := strings.IndexByte(code, '.'); i >= 0 {
		return code[:i]
	}
	return code
}

// ParseNumber parses an OEWS numeric cell. BLS uses *, **, #, ~ and blanks
// for suppressed, unavailable or top-coded values; those return ok=false.
func ParseNumber(s string) (float64, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

var spaceRe = regexp.MustCompile(`\s+`)

// NormTask normalises task text for matching across datasets.
func NormTask(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = spaceRe.ReplaceAllString(s, " ")
	return strings.TrimRight(s, ". ")
}
