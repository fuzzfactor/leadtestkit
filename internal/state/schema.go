// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SchemaVersion must be bumped on every change of the state structure,
// even an added field: older versions refuse unknown fields, and a
// version mismatch tells the user what to do.  TestSchemaFields fails
// when the structure changes without a bump.
const SchemaVersion = 10

const (
	kindMeta   = "meta"
	kindSeries = "series"
)

// migrations[v] upgrades a file from schema v to v+1, working on the
// raw JSON before it is decoded.  Versions without an entry only added
// fields and need no conversion.
var migrations = map[int]func(kind string, doc map[string]any) error{
	3: skipUnfinishedPatches,
	4: dropUnpushedBranches,
	5: linkRunDecisions,
	7: keyOutages,
	9: splitCheckFailures,
}

// splitCheckFailures moves the retry delays of pending checks, which
// delayed all reports of their runs before schema 10, to their own
// field.
func splitCheckFailures(kind string, doc map[string]any) error {
	if kind != kindSeries {
		return nil
	}
	list, _ := doc["series"].([]any)
	for _, x := range list {
		s, _ := x.(map[string]any)
		patches, _ := s["patches"].([]any)
		for _, y := range patches {
			p, _ := y.(map[string]any)
			runs, _ := p["runs"].([]any)
			for _, z := range runs {
				r, _ := z.(map[string]any)
				f, _ := r["failure"].(map[string]any)
				if f != nil && f["op"] == "pending check" {
					r["check_failure"] = f
					delete(r, "failure")
				}
			}
		}
	}
	return nil
}

// keyOutages moves the outage of workflows, the only kind before
// schema 8, into the outages by kind.
func keyOutages(kind string, doc map[string]any) error {
	o, _ := doc["ci_outage"].(map[string]any)
	if kind != kindMeta || o == nil {
		return nil
	}
	delete(doc, "ci_outage")
	k, _ := o["kind"].(string)
	delete(o, "kind")
	if k == "" {
		k = OutageRuns
	}
	doc["outages"] = map[string]any{k: o}
	return nil
}

var runDecisionRe = regexp.MustCompile(`\brun (\d+)(?:, attempt (\d+))?`)

// linkRunDecisions links the decisions about a workflow run to the run,
// which schema 6 records, and adds the warnings to the decisions that
// before only counted them.
func linkRunDecisions(kind string, doc map[string]any) error {
	if kind != kindSeries {
		return nil
	}
	list, _ := doc["series"].([]any)
	for _, x := range list {
		s, _ := x.(map[string]any)
		patches, _ := s["patches"].([]any)
		for _, y := range patches {
			p, _ := y.(map[string]any)
			if p == nil {
				continue
			}
			runs := map[string]map[string]any{}
			rs, _ := p["runs"].([]any)
			for _, z := range rs {
				if r, _ := z.(map[string]any); r != nil && r["id"] != nil {
					runs[fmt.Sprint(r["id"])] = r
				}
			}
			ds, _ := p["decisions"].([]any)
			for _, z := range ds {
				d, _ := z.(map[string]any)
				if d == nil {
					continue
				}
				detail, _ := d["detail"].(string)
				m := runDecisionRe.FindStringSubmatch(detail)
				if m == nil || runs[m[1]] == nil {
					continue
				}
				r := runs[m[1]]
				if u, _ := r["url"].(string); u != "" {
					if m[2] != "" {
						u += "/attempts/" + m[2]
					}
					d["workflow_url"] = u
				}
				details, _ := r["details"].(string)
				if d["action"] == "workflow-warnings" &&
					strings.HasPrefix(details, "Warnings:\n") {
					d["detail"] = strings.TrimSuffix(detail, ".") + ":\n" +
						strings.TrimPrefix(details, "Warnings:\n")
				}
			}
		}
	}
	return nil
}

// dropUnpushedBranches removes the test branch of series that never
// pushed anything: before schema 5 it was recorded before pushing.
func dropUnpushedBranches(kind string, doc map[string]any) error {
	if kind != kindSeries {
		return nil
	}
	list, _ := doc["series"].([]any)
	for _, x := range list {
		s, _ := x.(map[string]any)
		if s == nil {
			continue
		}
		pushed := false
		patches, _ := s["patches"].([]any)
		for _, y := range patches {
			if p, _ := y.(map[string]any); p != nil && p["pushed"] != nil &&
				p["pushed"] != "" {
				pushed = true
			}
		}
		if !pushed {
			delete(s, "branch")
		}
	}
	return nil
}

// skipUnfinishedPatches gives the patches of series skipped or failed
// before schema 4 the status Series.Finish now sets.
func skipUnfinishedPatches(kind string, doc map[string]any) error {
	if kind != kindSeries {
		return nil
	}
	list, _ := doc["series"].([]any)
	for _, x := range list {
		s, _ := x.(map[string]any)
		if s == nil || (s["status"] != SeriesSkipped && s["status"] != SeriesError) {
			continue
		}
		patches, _ := s["patches"].([]any)
		for _, y := range patches {
			p, _ := y.(map[string]any)
			if p == nil {
				continue
			}
			switch p["status"] {
			case PatchPending, PatchApplied, PatchPushed:
				p["status"] = PatchSkipped
			}
		}
	}
	return nil
}

// readState reads a state file of the given kind, upgrading it to the
// current schema.  It reports whether the file was upgraded and has to
// be written back.
func readState(path, kind string, dest any) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	name := filepath.Base(path)
	if filepath.Base(filepath.Dir(path)) == HistoryDir {
		name = HistoryDir + "/" + name
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	version := 1
	if v, ok := doc["schema"]; ok {
		n, ok := v.(json.Number)
		i, err := n.Int64()
		if !ok || err != nil || i < 1 {
			return false, fmt.Errorf("%s: invalid schema %v", name, v)
		}
		version = int(i)
	}
	if version > SchemaVersion {
		return false, fmt.Errorf("%s: state schema %d is newer than %d supported by this "+
			"version of leadtest; update leadtestkit", name, version, SchemaVersion)
	}
	if err := migrate(doc, kind, version, SchemaVersion, migrations); err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	data, err = json.Marshal(doc)
	if err != nil {
		return false, err
	}
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(dest); err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return version != SchemaVersion, nil
}

func migrate(doc map[string]any, kind string, from, to int,
	steps map[int]func(string, map[string]any) error) error {
	for v := from; v < to; v++ {
		if m := steps[v]; m != nil {
			if err := m(kind, doc); err != nil {
				return fmt.Errorf("migrating from schema %d: %w", v, err)
			}
		}
	}
	doc["schema"] = to
	return nil
}

// Instance identifies what a state belongs to.  Using a state with
// another Patchwork project or test repository would mix up cursors,
// series and test refs.
type Instance struct {
	Patchwork string `json:"patchwork"`
	Project   string `json:"project"`
	TestRepo  string `json:"test_repo"`
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/"))
	return strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
}

// CheckInstance records the instance on first use and refuses a state
// that belongs to another one.
func (s *Store) CheckInstance(want Instance) error {
	cur := s.Meta.Instance
	if cur == nil {
		s.Meta.Instance = &want
		return nil
	}
	if normalize(cur.Patchwork) != normalize(want.Patchwork) ||
		!strings.EqualFold(cur.Project, want.Project) ||
		!strings.EqualFold(cur.TestRepo, want.TestRepo) {
		return fmt.Errorf("the state belongs to %s project %s with test repository "+
			"%s, but config.yaml uses %s project %s with %s; fix config.yaml, or "+
			"update or remove \"instance\" in meta.json if the change is intended",
			cur.Patchwork, cur.Project, cur.TestRepo,
			want.Patchwork, want.Project, want.TestRepo)
	}
	return nil
}
