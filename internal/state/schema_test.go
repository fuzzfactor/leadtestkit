// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "update the state schema fingerprint")

// fields lists every JSON field path of the state files.
func fields(t reflect.Type, prefix string, out *[]string) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		if t.Kind() == reflect.Map {
			prefix += "{}"
		} else if t.Kind() == reflect.Slice {
			prefix += "[]"
		}
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		*out = append(*out, prefix+" "+t.Kind().String())
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || !f.IsExported() {
			continue
		}
		fields(f.Type, prefix+"."+name, out)
	}
}

func TestSchemaFields(t *testing.T) {
	var got []string
	fields(reflect.TypeOf(Meta{}), "meta", &got)
	fields(reflect.TypeOf(queueFile{}), "series", &got)
	sort.Strings(got)
	text := strings.Join(got, "\n") + "\n"
	path := filepath.Join("testdata", fmt.Sprintf("schema-%d.txt", SchemaVersion))
	if *update {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil || string(want) != text {
		t.Fatalf("the state structure differs from %s.  Bump SchemaVersion, add a "+
			"migration if old files need converting, then run: "+
			"go test ./internal/state -run TestSchemaFields -update", path)
	}
}

func TestFilesCarrySchema(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	x := sampleSeries(1)
	s.Add(x)
	s.Add(sampleSeries(2))
	x.Finish(SeriesDone, "", now)
	s.Archive(x)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{MetaFile, QueueFile, filepath.Join(HistoryDir, "2026-03.json")} {
		data, _ := os.ReadFile(filepath.Join(dir, f))
		if !strings.HasPrefix(string(data),
			fmt.Sprintf("{\n  \"schema\": %d,", SchemaVersion)) {
			t.Errorf("%s does not start with the schema:\n%.80s", f, data)
		}
	}
}

func TestFileWithoutSchemaIsVersionOne(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, QueueFile),
		[]byte(`{"series":[{"id":1,"status":"new","discovered":"","name":"","date":"",`+
			`"submitter":{"email":""},"total":0,"patches":[]}]}`), 0o644)
	s, err := Load(dir)
	if err != nil || len(s.Queue) != 1 {
		t.Fatalf("queue=%v err=%v", s, err)
	}
}

func TestVersionTwoRecordsHaveNoInventedDecisions(t *testing.T) {
	dir := t.TempDir()
	x := sampleSeries(1)
	b, err := json.Marshal(queueFile{Schema: 2, Series: []*Series{x}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, QueueFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Queue[0].Decisions) != 0 || len(s.Queue[0].Patches[0].Decisions) != 0 {
		t.Fatal("migration invented decision provenance")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
}

func TestVersionThreeSkipsPatchesOfFinishedSeries(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, HistoryDir), 0o755); err != nil {
		t.Fatal(err)
	}
	var list []*Series
	for i, status := range []string{SeriesSkipped, SeriesError, SeriesDone} {
		x := sampleSeries(i + 1)
		x.Status = status
		x.Finished = FormatTime(now)
		x.Patches[1].Status = PatchApplyFailed
		list = append(list, x)
	}
	b, err := json.Marshal(queueFile{Schema: 3, Series: list})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, HistoryDir, "2026-03.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.History("2026-03")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range got {
		want := PatchSkipped
		if x.Status == SeriesDone {
			want = PatchPending
		}
		if x.Patches[0].Status != want || x.Patches[1].Status != PatchApplyFailed {
			t.Errorf("%s series: patches %s, %s", x.Status,
				x.Patches[0].Status, x.Patches[1].Status)
		}
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"status": "skipped"`) {
		t.Error("the migrated history was not written back")
	}
}

func TestVersionFourDropsUnpushedBranches(t *testing.T) {
	dir := t.TempDir()
	never, pushed := sampleSeries(1), sampleSeries(2)
	for _, x := range []*Series{never, pushed} {
		x.Branch = fmt.Sprintf("series_%d", x.ID)
		x.Base = &Base{Branch: "main", SHA: "lorem"}
	}
	pushed.Patches[0].Pushed = FormatTime(now)
	b, err := json.Marshal(queueFile{Schema: 4, Series: []*Series{never, pushed}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, QueueFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Queue[0].Branch != "" || s.Queue[0].Base.Branch != "main" {
		t.Errorf("series without pushes: %+v, base %+v", s.Queue[0], s.Queue[0].Base)
	}
	if s.Queue[1].Branch != "series_2" {
		t.Errorf("the branch of a pushed series was dropped: %+v", s.Queue[1])
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, HistoryDir), 0o755)
	newer := fmt.Sprintf(`{"schema":%d,"series":[],"lorem":1}`, SchemaVersion+1)
	os.WriteFile(filepath.Join(dir, HistoryDir, "2026-01.json"), []byte(newer), 0o644)
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.History("2026-01")
	if err == nil || !strings.Contains(err.Error(), "history/2026-01.json: state schema") ||
		!strings.Contains(err.Error(), "update leadtestkit") {
		t.Fatalf("err = %v", err)
	}
	os.WriteFile(filepath.Join(dir, MetaFile), []byte(newer), 0o644)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "meta.json") {
		t.Fatalf("err = %v", err)
	}
}

func TestMigrate(t *testing.T) {
	steps := map[int]func(string, map[string]any) error{
		1: func(kind string, doc map[string]any) error {
			if kind == kindSeries {
				doc["series"] = doc["items"]
				delete(doc, "items")
			}
			return nil
		},
		3: func(kind string, doc map[string]any) error {
			return fmt.Errorf("lorem")
		},
	}
	doc := map[string]any{"items": []any{}}
	if err := migrate(doc, kindSeries, 1, 3, steps); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["series"]; !ok || doc["schema"] != 3 {
		t.Errorf("doc = %v", doc)
	}
	if err := migrate(map[string]any{}, kindMeta, 3, 4, steps); err == nil ||
		!strings.Contains(err.Error(), "from schema 3") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckInstance(t *testing.T) {
	s, _ := Load(t.TempDir())
	want := Instance{Patchwork: "https://pw.example.org", Project: "lorem",
		TestRepo: "github.com/ipsum/lorem"}
	if err := s.CheckInstance(want); err != nil || s.Meta.Instance == nil {
		t.Fatalf("err = %v", err)
	}
	same := Instance{Patchwork: "http://PW.example.org/", Project: "Lorem",
		TestRepo: "github.com/Ipsum/lorem"}
	if err := s.CheckInstance(same); err != nil {
		t.Errorf("err = %v", err)
	}
	other := want
	other.Project = "dolor"
	if err := s.CheckInstance(other); err == nil ||
		!strings.Contains(err.Error(), "meta.json") {
		t.Errorf("err = %v", err)
	}
	other = want
	other.TestRepo = "github.com/ipsum/other"
	if err := s.CheckInstance(other); err == nil {
		t.Error("a different test repository must be refused")
	}
}
