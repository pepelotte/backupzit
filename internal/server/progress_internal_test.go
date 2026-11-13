package server

import (
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

func TestEstimatePercent(t *testing.T) {
	for _, c := range []struct {
		files, prevFiles, scanned, prevBytes uint64
		want                                 int
		ok                                   bool
	}{
		{4, 10, 0, 0, 40, true},        // agent before 0.44: files only
		{4, 10, 800, 1000, 60, true},   // average of 40% and 80%
		{12, 10, 500, 1000, 75, true},  // more files than before: size still counts
		{12, 10, 1200, 1000, 0, false}, // both past the previous backup
		{0, 10, 0, 1000, 0, false},     // nothing yet
		{9, 10, 990, 1000, 94, true},   // average 94.5 → 94
		{10, 10, 999, 1000, 99, true},  // never 100 before the end
		{3, 0, 300, 0, 0, false},       // no previous backup
	} {
		got, ok := estimatePercent(c.files, c.prevFiles, c.scanned, c.prevBytes)
		if got != c.want || ok != c.ok {
			t.Errorf("%+v: got %d %v", c, got, ok)
		}
	}
}

// The speed is measured over the reports of the last minute.
func TestProgressSpeed(t *testing.T) {
	var p liveProgress
	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	rep := func(sec int, done uint64) {
		p.set(7, []api.RunProgress{{RunID: 1, Done: done}}, t0.Add(time.Duration(sec)*time.Second))
	}
	rep(0, 0)
	if bps, _ := p.speed(7, 1); bps != 0 {
		t.Errorf("first report: %d", bps)
	}
	rep(10, 10<<20)
	if bps, _ := p.speed(7, 1); bps != 1<<20 {
		t.Errorf("after 10 s: %d", bps)
	}
	for sec := 20; sec <= 120; sec += 10 {
		done := uint64(10 << 20)
		if sec > 60 {
			done += uint64(sec-60) * (3 << 20) // 3 MB/s from 60 s on
		}
		rep(sec, done)
	}
	if bps, at := p.speed(7, 1); bps != 3<<20 || !at.Equal(t0.Add(120*time.Second)) {
		t.Errorf("after the change: %d %v", bps, at)
	}
	if bps, _ := p.speed(8, 1); bps != 0 {
		t.Error("another agent's run")
	}
}
