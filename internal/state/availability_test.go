// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAvailability(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	a := s.Availability
	p := a.Unavailable(ServicePatchwork, "lorem", t0)
	if again := a.Unavailable(ServicePatchwork, "ipsum", t0.Add(time.Minute)); again != p ||
		p.Reason != "lorem" {
		t.Fatalf("second failure started a new period: %+v", again)
	}
	if a.Available(ServiceGitHub, t0) != nil {
		t.Fatal("ended a period that did not start")
	}
	a.Available(ServicePatchwork, t0.Add(10*time.Minute))
	a.Unavailable(ServiceEmail, "dolor", t0.Add(time.Hour))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a = s.Availability
	if len(a.Periods) != 1 || a.Periods[0].Until != "2026-03-10T12:10:00Z" ||
		a.Failing[ServiceEmail] == nil {
		t.Fatalf("availability = %+v", a)
	}
	a.Prune(t0.Add(11 * time.Minute))
	if len(a.Periods) != 0 || a.Failing[ServiceEmail] == nil {
		t.Fatalf("after pruning: %+v", a)
	}
}

func TestCountPatchworkTry(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 3, 10, 12, 59, 0, 0, time.UTC)
	a := s.Availability
	a.CountPatchworkTry(true, t0)
	a.CountPatchworkTry(false, t0.Add(30*time.Second))
	a.CountPatchworkTry(true, t0.Add(2*time.Minute))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []*HourTries{
		{Hour: "2026-03-10T12:00:00Z", Tries: 2, Failed: 1},
		{Hour: "2026-03-10T13:00:00Z", Tries: 1, Failed: 1},
	}
	if got := s.Availability.PatchworkHourlyTries; !reflect.DeepEqual(got, want) {
		t.Fatalf("tries = %s", toJSON(got))
	}
}

func TestPrunePatchworkTries(t *testing.T) {
	a := newAvailability()
	a.PatchworkHourlyTries = []*HourTries{
		{Hour: "2026-02-10T05:00:00Z", Tries: 9, Failed: 9},
		{Hour: "2026-03-12T22:00:00Z", Tries: 2, Failed: 1},
		{Hour: "2026-03-12T23:00:00Z", Tries: 3},
		{Hour: "2026-03-13T09:00:00Z", Tries: 4},
		{Hour: "2026-03-13T11:00:00Z", Tries: 5, Failed: 2},
	}
	a.PatchworkDailyTries = []*DayTries{
		{Day: "2026-02-16", Tries: 7},
		{Day: "2026-02-17", Tries: 8, Failed: 1},
		{Day: "2026-03-12", Tries: 1},
	}
	now := time.Date(2026, 3, 20, 10, 30, 0, 0, time.UTC)
	a.PrunePatchworkTries(now, 7*24*time.Hour, 31*24*time.Hour)
	wantDays := []*DayTries{
		{Day: "2026-02-17", Tries: 8, Failed: 1},
		{Day: "2026-03-12", Tries: 6, Failed: 1},
	}
	if !reflect.DeepEqual(a.PatchworkDailyTries, wantDays) {
		t.Fatalf("days = %s", toJSON(a.PatchworkDailyTries))
	}
	wantHours := []*HourTries{
		{Hour: "2026-03-13T09:00:00Z", Tries: 4},
		{Hour: "2026-03-13T11:00:00Z", Tries: 5, Failed: 2},
	}
	if !reflect.DeepEqual(a.PatchworkHourlyTries, wantHours) {
		t.Fatalf("hours = %s", toJSON(a.PatchworkHourlyTries))
	}
}

func TestAvailabilityWithoutTries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AvailabilityFile)
	err := os.WriteFile(path, []byte(`{"schema":14,"failing":{},"periods":[]}`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Availability
	if len(a.PatchworkHourlyTries) != 0 || len(a.PatchworkDailyTries) != 0 {
		t.Fatalf("tries = %s", toJSON(a))
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "patchwork_") {
		t.Fatalf("saved: %s", b)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
