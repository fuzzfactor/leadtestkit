// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	MetaFile   = "meta.json"
	QueueFile  = "queue.json"
	HistoryDir = "history"
)

type queueFile struct {
	Series []*Series `json:"series"`
}

// Store is the bot state kept as JSON files in a git checkout.
type Store struct {
	Dir   string
	Meta  *Meta
	Queue []*Series

	history map[string][]*Series
	dirty   map[string]bool
}

func newMeta() *Meta {
	return &Meta{
		Schema:   SchemaVersion,
		Events:   map[string]int{},
		Archives: map[string]*ArchiveCursor{},
	}
}

func Load(dir string) (*Store, error) {
	s := &Store{Dir: dir, history: map[string][]*Series{}, dirty: map[string]bool{}}
	s.Meta = newMeta()
	if err := readJSON(filepath.Join(dir, MetaFile), s.Meta); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.Meta.Schema > SchemaVersion {
		return nil, fmt.Errorf("state schema %d is newer than supported %d", s.Meta.Schema, SchemaVersion)
	}
	s.Meta.Schema = SchemaVersion
	if s.Meta.Events == nil {
		s.Meta.Events = map[string]int{}
	}
	if s.Meta.Archives == nil {
		s.Meta.Archives = map[string]*ArchiveCursor{}
	}
	var q queueFile
	if err := readJSON(filepath.Join(dir, QueueFile), &q); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	s.Queue = q.Series
	if err := validate(QueueFile, s.Queue); err != nil {
		return nil, err
	}
	return s, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

var validSeriesStatus = map[string]bool{
	SeriesNew: true, SeriesQueued: true, SeriesWaiting: true, SeriesTesting: true,
	SeriesDone: true, SeriesSkipped: true, SeriesError: true,
}

var validPatchStatus = map[string]bool{
	PatchPending: true, PatchApplied: true, PatchApplyFailed: true, PatchNotApplied: true,
	PatchPushed: true, PatchDone: true, PatchSkipped: true,
}

func validate(name string, list []*Series) error {
	seen := map[int]bool{}
	for i, s := range list {
		if s == nil {
			return fmt.Errorf("%s: series[%d] is null", name, i)
		}
		if s.ID <= 0 || seen[s.ID] {
			return fmt.Errorf("%s: series[%d]: invalid or duplicate id %d", name, i, s.ID)
		}
		seen[s.ID] = true
		if !validSeriesStatus[s.Status] {
			return fmt.Errorf("%s: series %d: invalid status %q", name, s.ID, s.Status)
		}
		for j, p := range s.Patches {
			if p == nil || p.ID <= 0 {
				return fmt.Errorf("%s: series %d: patch[%d] invalid", name, s.ID, j)
			}
			if !validPatchStatus[p.Status] {
				return fmt.Errorf("%s: patch %d: invalid status %q", name, p.ID, p.Status)
			}
			for k, r := range p.Runs {
				if r == nil {
					return fmt.Errorf("%s: patch %d: run[%d] is null", name, p.ID, k)
				}
			}
		}
	}
	return nil
}

// Marshal produces the stable, human-diffable form of the state.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeJSON(path string, v any) error {
	data, err := Marshal(v)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sortSeries(list []*Series) {
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
}

func (s *Store) Save() error {
	sortSeries(s.Queue)
	if s.Queue == nil {
		s.Queue = []*Series{}
	}
	if err := writeJSON(filepath.Join(s.Dir, MetaFile), s.Meta); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.Dir, QueueFile), queueFile{Series: s.Queue}); err != nil {
		return err
	}
	for month := range s.dirty {
		list := s.history[month]
		sortSeries(list)
		if err := writeJSON(s.historyPath(month), queueFile{Series: list}); err != nil {
			return err
		}
	}
	s.dirty = map[string]bool{}
	return nil
}

func (s *Store) historyPath(month string) string {
	return filepath.Join(s.Dir, HistoryDir, month+".json")
}

func (s *Store) Find(id int) *Series {
	for _, x := range s.Queue {
		if x.ID == id {
			return x
		}
	}
	return nil
}

func (s *Store) Add(x *Series) {
	s.Queue = append(s.Queue, x)
	sortSeries(s.Queue)
}

// HistoryMonths lists history shards, newest first.
func (s *Store) HistoryMonths() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, HistoryDir))
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			set[name] = true
		}
	}
	for m := range s.history {
		set[m] = true
	}
	months := make([]string, 0, len(set))
	for m := range set {
		months = append(months, m)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(months)))
	return months, nil
}

func (s *Store) History(month string) ([]*Series, error) {
	if list, ok := s.history[month]; ok {
		return list, nil
	}
	var h queueFile
	if err := readJSON(s.historyPath(month), &h); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := validate(month, h.Series); err != nil {
		return nil, err
	}
	s.history[month] = h.Series
	return h.Series, nil
}

// MonthOf returns the history shard of a finished series.
func MonthOf(x *Series) string {
	t := ParseTime(x.Finished)
	if t.IsZero() {
		t = ParseTime(x.Discovered)
	}
	return t.UTC().Format("2006-01")
}

// Archive moves a finished series from the queue to its history shard.
func (s *Store) Archive(x *Series) error {
	month := MonthOf(x)
	list, err := s.History(month)
	if err != nil {
		return err
	}
	for i, q := range s.Queue {
		if q == x {
			s.Queue = append(s.Queue[:i], s.Queue[i+1:]...)
			break
		}
	}
	replaced := false
	for i, h := range list {
		if h.ID == x.ID {
			list[i] = x
			replaced = true
		}
	}
	if !replaced {
		list = append(list, x)
	}
	s.history[month] = list
	s.dirty[month] = true
	return nil
}

// Reopen moves a series from history back to the queue.
func (s *Store) Reopen(x *Series) error {
	month := MonthOf(x)
	list, err := s.History(month)
	if err != nil {
		return err
	}
	for i, h := range list {
		if h.ID == x.ID {
			s.history[month] = append(list[:i], list[i+1:]...)
			s.dirty[month] = true
			break
		}
	}
	x.Finished = ""
	s.Add(x)
	return nil
}

// Recent returns queued series plus history series finished within the
// window, newest shards first.
func (s *Store) Recent(now time.Time, window time.Duration) ([]*Series, error) {
	out := append([]*Series(nil), s.Queue...)
	cutoff := now.Add(-window)
	months, err := s.HistoryMonths()
	if err != nil {
		return nil, err
	}
	for _, m := range months {
		t, err := time.Parse("2006-01", m)
		if err != nil || t.AddDate(0, 1, 0).Before(cutoff) {
			continue
		}
		list, err := s.History(m)
		if err != nil {
			return nil, err
		}
		for _, x := range list {
			if f := ParseTime(x.Finished); !f.IsZero() && f.Before(cutoff) {
				continue
			}
			out = append(out, x)
		}
	}
	return out, nil
}

// FindPatch looks for a patch among the given series.
func FindPatch(list []*Series, patchID int) (*Series, *Patch) {
	for _, x := range list {
		if p := x.Patch(patchID); p != nil {
			return x, p
		}
	}
	return nil, nil
}

func (s *Store) Cursor(name string) *ArchiveCursor {
	c := s.Meta.Archives[name]
	if c == nil {
		c = &ArchiveCursor{Months: map[string]int{}}
		s.Meta.Archives[name] = c
	}
	if c.Months == nil {
		c.Months = map[string]int{}
	}
	return c
}
