// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"testing"
	"time"

	"leadtestkit/internal/config"
	"leadtestkit/internal/state"
)

func TestHistoryLookupsAreUnlimitedByDefault(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.step()
	s := e.series(7)
	s.Finish(state.SeriesDone, "", e.now)
	if err := e.p.Store.Archive(s); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Store.Save(); err != nil {
		t.Fatal(err)
	}
	e.advance(60 * 24 * time.Hour)
	e.reload()
	delete(e.pw.patches, 701)
	ids, err := e.p.known()
	if err != nil || !ids[7] {
		t.Fatalf("old series was forgotten: %v, %v", ids, err)
	}
	ok, _, err := e.p.ourPatch(context.Background(), 701)
	if err != nil || !ok {
		t.Fatalf("old patch was not recognized from history: %t, %v", ok, err)
	}
	e.p.RunURL = nextBotRun
	e.p.noteCheck(701, "check-posted", "A late report was posted.")
	if err := e.p.Store.Save(); err != nil {
		t.Fatal(err)
	}
	e.reload()
	decision(t, e.series(7).Patches[0].Decisions, "check-posted", nextBotRun)

	e.cfg.Limits.HistoryWindow = config.Duration(24 * time.Hour)
	ids, err = e.p.known()
	if err != nil || ids[7] {
		t.Fatalf("finite history window was ignored: %v, %v", ids, err)
	}
	ok, _, err = e.p.ourPatch(context.Background(), 701)
	if err != nil || ok {
		t.Fatalf("finite window recognized a patch outside history: %t, %v", ok, err)
	}
}
