package server

import (
	"testing"
	"time"
)

// TestScheduleAPIFields covers the schedule fields set through the API:
// months, a period, the week of the month, a window with minutes and
// since_last.
func TestScheduleAPIFields(t *testing.T) {
	loc := time.Local
	at := func(y int, mo time.Month, d, h, m int) time.Time { return time.Date(y, mo, d, h, m, 0, 0, loc) }
	now := at(2026, 10, 7, 10, 30) // Wednesday

	cases := []struct {
		name string
		s    Schedule
		next time.Time
		desc string
	}{
		{"months", Schedule{Kind: SchedDaily, Times: []string{"22:00"}, Months: []int{1, 4, 7, 10}},
			at(2026, 10, 7, 22, 0), "Every day at 22:00; months 1, 4, 7, 10"},
		{"months skip", Schedule{Kind: SchedDaily, Times: []string{"22:00"}, Months: []int{12, 3}},
			at(2026, 12, 1, 22, 0), "Every day at 22:00; months 3, 12"},
		{"all months is none", Schedule{Kind: SchedDaily, Times: []string{"22:00"}, Months: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
			at(2026, 10, 7, 22, 0), "Every day at 22:00"},
		{"period start", Schedule{Kind: SchedDaily, Times: []string{"06:00"}, ValidFrom: "2026-11-02", ValidUntil: "2026-11-30"},
			at(2026, 11, 2, 6, 0), "Every day at 06:00; from 2026-11-02; until 2026-11-30"},
		{"period over", Schedule{Kind: SchedDaily, Times: []string{"06:00"}, ValidUntil: "2026-10-06"},
			time.Time{}, "Every day at 06:00; until 2026-10-06"},
		{"first monday", Schedule{Kind: SchedMonthly, Week: 1, Weekday: 1, Times: []string{"23:00"}},
			at(2026, 11, 2, 23, 0), "Monthly on the first Mon at 23:00"},
		{"last friday", Schedule{Kind: SchedMonthly, Week: 5, Weekday: 5, Times: []string{"20:00"}},
			at(2026, 10, 30, 20, 0), "Monthly on the last Fri at 20:00"},
		{"second sunday in january", Schedule{Kind: SchedMonthly, Week: 2, Weekday: 0, Times: []string{"03:00"}, Months: []int{1}},
			at(2027, 1, 10, 3, 0), "Monthly on the second Sun at 03:00; months 1"},
		{"minute window", Schedule{Kind: SchedInterval, EveryMinutes: 120, FromTime: "08:30", ToTime: "17:45", Days: []int{1, 2, 3, 4, 5}},
			at(2026, 10, 7, 10, 30).Add(2 * time.Hour), "Every 2 hours between 08:30 and 17:45, Mon–Fri"},
		{"since last", Schedule{Kind: SchedSinceLast, EveryHours: 36},
			now.Add(36 * time.Hour), "Every 36 h after the previous backup"},
		{"since last on weekdays", Schedule{Kind: SchedSinceLast, EveryHours: 12, Days: []int{1, 2, 3, 4, 5}},
			at(2026, 10, 7, 22, 30), "Every 12 h after the previous backup, Mon–Fri"},
	}
	for _, c := range cases {
		s := c.s
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		// Through the stored form, as the scheduler sees it.
		p, err := ParseSchedule(s.Encode())
		if err != nil {
			t.Errorf("%s: parse: %v", c.name, err)
			continue
		}
		if got := p.Next(now); !got.Equal(c.next) {
			t.Errorf("%s: next %v, want %v", c.name, got, c.next)
		}
		if got := p.Describe(); got != c.desc {
			t.Errorf("%s: %q, want %q", c.name, got, c.desc)
		}
	}

	// The window with minutes: 08:30, 10:30 … 16:30, then the next weekday.
	s := Schedule{Kind: SchedInterval, EveryMinutes: 120, FromTime: "08:30", ToTime: "17:45", Days: []int{1, 2, 3, 4, 5}}
	if err := s.Validate(); err != nil || s.FromHour != 8 || s.ToHour != 18 {
		t.Fatalf("window: %v, hours %d-%d", err, s.FromHour, s.ToHour)
	}
	if got := s.Next(at(2026, 10, 9, 16, 30)); !got.Equal(at(2026, 10, 12, 8, 30)) { // Friday → Monday
		t.Errorf("after the last slot of Friday: %v", got)
	}
	if got := (Schedule{Kind: SchedSinceLast, EveryHours: 12, Days: []int{1, 2, 3, 4, 5}}).Next(at(2026, 10, 9, 20, 0)); !got.Equal(at(2026, 10, 12, 0, 0)) {
		t.Errorf("since_last over the weekend: %v", got)
	}

	for name, bad := range map[string]Schedule{
		"month 13":        {Kind: SchedDaily, Times: []string{"22:00"}, Months: []int{13}},
		"bad date":        {Kind: SchedDaily, Times: []string{"22:00"}, ValidFrom: "1.1.2027"},
		"until < from":    {Kind: SchedDaily, Times: []string{"22:00"}, ValidFrom: "2027-02-01", ValidUntil: "2027-01-01"},
		"week 6":          {Kind: SchedMonthly, Week: 6, Times: []string{"22:00"}},
		"weekday no week": {Kind: SchedMonthly, Weekday: 3, DayOfMonth: 1, Times: []string{"22:00"}},
		"window reversed": {Kind: SchedInterval, EveryMinutes: 60, FromTime: "18:00", ToTime: "08:00"},
		"window half":     {Kind: SchedInterval, EveryMinutes: 60, FromTime: "08:00"},
		"since 0":         {Kind: SchedSinceLast},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Schedules that do not use them drop the fields.
	ch := Schedule{Kind: SchedChain, AfterJob: 3, Months: []int{1}, ValidFrom: "2026-01-01", Week: 1, EveryHours: 4}
	if err := ch.Validate(); err != nil || ch.Months != nil || ch.ValidFrom != "" || ch.Week != 0 || ch.EveryHours != 0 {
		t.Errorf("chain: %v %+v", err, ch)
	}
}

// TestKeepAPIScheduleFields: saving a job in the console keeps what the
// API set, unless the form changed the parts it shows.
func TestKeepAPIScheduleFields(t *testing.T) {
	enc := func(s Schedule) string {
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
		return s.Encode()
	}
	old := enc(Schedule{Kind: SchedMonthly, Week: 1, Weekday: 1, Times: []string{"23:00"}, Months: []int{3, 6}, ValidUntil: "2027-12-31"})
	// The form shows day 1 for it; saved unchanged, the weekday stays.
	got, _ := ParseSchedule(keepAPIScheduleFields(old, enc(Schedule{Kind: SchedMonthly, DayOfMonth: 1, Times: []string{"23:00"}})))
	if got.Week != 1 || got.Weekday != 1 || got.DayOfMonth != 0 || len(got.Months) != 2 || got.ValidUntil != "2027-12-31" {
		t.Errorf("kept: %+v", got)
	}
	// Another day chosen: the weekday goes, months and period stay.
	got, _ = ParseSchedule(keepAPIScheduleFields(old, enc(Schedule{Kind: SchedMonthly, DayOfMonth: 15, Times: []string{"23:00"}})))
	if got.Week != 0 || got.DayOfMonth != 15 || len(got.Months) != 2 {
		t.Errorf("day 15: %+v", got)
	}
	// Another kind: nothing carried over.
	got, _ = ParseSchedule(keepAPIScheduleFields(old, enc(Schedule{Kind: SchedDaily, Times: []string{"22:00"}})))
	if got.Months != nil || got.ValidUntil != "" {
		t.Errorf("daily: %+v", got)
	}
	iv := enc(Schedule{Kind: SchedInterval, EveryMinutes: 60, FromTime: "07:30", ToTime: "19:15"})
	got, _ = ParseSchedule(keepAPIScheduleFields(iv, enc(Schedule{Kind: SchedInterval, EveryMinutes: 60, FromHour: 7, ToHour: 20})))
	if got.FromTime != "07:30" || got.ToTime != "19:15" {
		t.Errorf("interval kept: %+v", got)
	}
	got, _ = ParseSchedule(keepAPIScheduleFields(iv, enc(Schedule{Kind: SchedInterval, EveryMinutes: 60, FromHour: 6, ToHour: 20})))
	if got.FromTime != "" || got.FromHour != 6 {
		t.Errorf("interval changed: %+v", got)
	}
	sl := enc(Schedule{Kind: SchedSinceLast, EveryHours: 30, Days: []int{1, 2, 3}})
	got, _ = ParseSchedule(keepAPIScheduleFields(sl, enc(Schedule{Kind: SchedSinceLast, EveryHours: 30})))
	if got.EveryHours != 30 || len(got.Days) != 3 {
		t.Errorf("since_last: %+v", got)
	}
}
