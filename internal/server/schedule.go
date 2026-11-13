package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schedule kinds, as offered in the console.
const (
	SchedManual   = "manual"
	SchedDaily    = "daily"    // at fixed times on selected weekdays
	SchedInterval = "interval" // every N minutes, optionally within a time window
	SchedMonthly  = "monthly"  // on a day of the month at a fixed time
	SchedAfter    = "after"    // copy jobs: after each backup of the source job
	SchedChain    = "chain"    // after another job finished (a chain: database, then files)
	// SchedSinceLast runs a fixed number of hours after the previous
	// scheduled run (set through the API).
	SchedSinceLast = "since_last"
)

// Schedule is a human-friendly backup schedule. It is stored as JSON in
// jobs.schedule and converted to cron expressions internally.
type Schedule struct {
	Kind string `json:"kind"`
	// Times are "HH:MM" (daily: all of them; monthly: the first).
	Times []string `json:"times,omitempty"`
	// Days are weekdays 0=Sunday..6=Saturday; empty means every day.
	Days []int `json:"days,omitempty"`
	// EveryMinutes is the interval for SchedInterval (15, 30, 60, 120, ...).
	EveryMinutes int `json:"every_minutes,omitempty"`
	// From/To limit an interval schedule to a window of whole hours
	// (From inclusive, To exclusive). Both zero means all day.
	FromHour int `json:"from_hour,omitempty"`
	ToHour   int `json:"to_hour,omitempty"`
	// DayOfMonth for SchedMonthly (1..28).
	DayOfMonth int `json:"day_of_month,omitempty"`
	// AfterJob is the job a SchedChain job follows; AfterAny starts it also
	// when that job failed.
	AfterJob int64 `json:"after_job,omitempty"`
	AfterAny bool  `json:"after_any,omitempty"`

	// The fields below are set through the API; the console's form keeps
	// them (see keepAPIScheduleFields).

	// Months limits daily, interval, monthly and since_last schedules to
	// these months (1..12); empty means every month.
	Months []int `json:"months,omitempty"`
	// Week (1..4, 5 = last) with Weekday (0=Sunday..6) runs a monthly
	// schedule on e.g. the first Monday instead of DayOfMonth.
	Week    int `json:"week,omitempty"`
	Weekday int `json:"weekday,omitempty"`
	// FromTime/ToTime ("HH:MM", ToTime may be "24:00") limit an interval
	// schedule to a window with minutes; runs start at FromTime and then
	// every EveryMinutes. They replace FromHour/ToHour.
	FromTime string `json:"from_time,omitempty"`
	ToTime   string `json:"to_time,omitempty"`
	// EveryHours is the gap of SchedSinceLast (1..8760).
	EveryHours int `json:"every_hours,omitempty"`
	// ValidFrom and ValidUntil ("YYYY-MM-DD", both days included) limit
	// the schedule to a period.
	ValidFrom  string `json:"valid_from,omitempty"`
	ValidUntil string `json:"valid_until,omitempty"`

	// cron holds a raw cron expression for schedules created before the
	// friendly format existed.
	cron string
}

// ParseSchedule decodes a stored schedule. Empty means manual; a value
// that is not JSON is treated as a cron expression.
func ParseSchedule(s string) (Schedule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Schedule{Kind: SchedManual}, nil
	}
	if !strings.HasPrefix(s, "{") {
		if _, err := cronParser.Parse(s); err != nil {
			return Schedule{}, fmt.Errorf("invalid schedule %q: %w", s, err)
		}
		if sc, ok := dailyFromCron(s); ok {
			return sc, nil
		}
		return Schedule{Kind: "cron", cron: s}, nil
	}
	var sc Schedule
	if err := json.Unmarshal([]byte(s), &sc); err != nil {
		return Schedule{}, fmt.Errorf("invalid schedule: %w", err)
	}
	return sc, sc.Validate()
}

// dailyFromCron converts "M H * * DOW" (DOW "*" or a list of 0-6) from
// older jobs into the friendly daily form.
func dailyFromCron(expr string) (Schedule, bool) {
	f := strings.Fields(expr)
	if len(f) != 5 || f[2] != "*" || f[3] != "*" {
		return Schedule{}, false
	}
	m, err1 := strconv.Atoi(f[0])
	h, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || m < 0 || m > 59 || h < 0 || h > 23 {
		return Schedule{}, false
	}
	sc := Schedule{Kind: SchedDaily, Times: []string{fmt.Sprintf("%02d:%02d", h, m)}}
	if f[4] != "*" {
		for _, d := range strings.Split(f[4], ",") {
			n, err := strconv.Atoi(d)
			if err != nil || n < 0 || n > 6 {
				return Schedule{}, false
			}
			sc.Days = append(sc.Days, n)
		}
	}
	return sc, sc.Validate() == nil
}

// Encode returns the stored form ("" for manual).
func (s Schedule) Encode() string {
	if s.cron != "" {
		return s.cron
	}
	if s.Kind == SchedManual || s.Kind == "" {
		return ""
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func parseHM(v string) (h, m int, err error) {
	hs, ms, ok := strings.Cut(strings.TrimSpace(v), ":")
	if ok {
		h, err = strconv.Atoi(hs)
		if err == nil {
			m, err = strconv.Atoi(ms)
		}
	}
	if !ok || err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid time %q (use HH:MM)", v)
	}
	return h, m, nil
}

// Validate checks the schedule and normalizes days and times.
func (s *Schedule) Validate() error {
	for _, d := range s.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("invalid weekday %d", d)
		}
	}
	sort.Ints(s.Days)
	if len(s.Days) == 7 {
		s.Days = nil
	}
	switch s.Kind {
	case SchedManual, SchedAfter, SchedChain:
		s.Months, s.ValidFrom, s.ValidUntil = nil, "", ""
	default:
		if err := s.validatePeriod(); err != nil {
			return err
		}
	}
	if s.Kind != SchedMonthly {
		s.Week, s.Weekday = 0, 0
	}
	if s.Kind != SchedInterval {
		s.FromTime, s.ToTime = "", ""
	}
	if s.Kind != SchedSinceLast {
		s.EveryHours = 0
	}
	switch s.Kind {
	case SchedManual:
		return nil
	case SchedAfter:
		s.Days, s.Times = nil, nil
		return nil
	case SchedChain:
		s.Days, s.Times = nil, nil
		if s.AfterJob <= 0 {
			return errors.New("choose the job to run after")
		}
		return nil
	case SchedSinceLast:
		s.Times = nil
		if s.EveryHours < 1 || s.EveryHours > 8760 {
			return errors.New("the hours after the previous backup must be 1 to 8760")
		}
	case SchedDaily:
		if len(s.Times) == 0 {
			return errors.New("choose at least one time")
		}
		seen := map[string]bool{}
		var times []string
		for _, t := range s.Times {
			h, m, err := parseHM(t)
			if err != nil {
				return err
			}
			n := fmt.Sprintf("%02d:%02d", h, m)
			if !seen[n] {
				seen[n] = true
				times = append(times, n)
			}
		}
		sort.Strings(times)
		s.Times = times
	case SchedInterval:
		if s.EveryMinutes <= 0 || (s.EveryMinutes < 60 && 60%s.EveryMinutes != 0) ||
			(s.EveryMinutes >= 60 && (s.EveryMinutes%60 != 0 || 24%(s.EveryMinutes/60) != 0)) {
			return fmt.Errorf("unsupported interval of %d minutes", s.EveryMinutes)
		}
		if s.FromHour < 0 || s.FromHour > 23 || s.ToHour < 0 || s.ToHour > 24 {
			return errors.New("invalid time window")
		}
		if s.FromHour != 0 || s.ToHour != 0 {
			if s.ToHour <= s.FromHour {
				return errors.New("the time window must end after it starts")
			}
		}
		if s.FromTime != "" || s.ToTime != "" {
			from, to, err := s.minuteWindow()
			if err != nil {
				return err
			}
			s.FromTime, s.ToTime = fmt.Sprintf("%02d:%02d", from/60, from%60), fmt.Sprintf("%02d:%02d", to/60, to%60)
			// The hours shown by the console's form.
			s.FromHour, s.ToHour = from/60, (to+59)/60
		}
	case SchedMonthly:
		if s.Week != 0 {
			if s.Week < 1 || s.Week > 5 || s.Weekday < 0 || s.Weekday > 6 {
				return errors.New("week must be 1 to 4 or 5 (last), weekday 0 (Sunday) to 6")
			}
			s.DayOfMonth = 0
		} else if s.Weekday != 0 {
			return errors.New("weekday needs week (1 to 4, or 5 for the last)")
		} else if s.DayOfMonth < 1 || s.DayOfMonth > 28 {
			return errors.New("day of month must be between 1 and 28")
		}
		if len(s.Times) == 0 {
			return errors.New("choose a time")
		}
		h, m, err := parseHM(s.Times[0])
		if err != nil {
			return err
		}
		s.Times = []string{fmt.Sprintf("%02d:%02d", h, m)}
		s.Days = nil
	default:
		return fmt.Errorf("unknown schedule type %q", s.Kind)
	}
	return nil
}

func (s Schedule) dowField() string {
	if len(s.Days) == 0 {
		return "*"
	}
	parts := make([]string, len(s.Days))
	for i, d := range s.Days {
		parts[i] = strconv.Itoa(d)
	}
	return strings.Join(parts, ",")
}

// crons converts the schedule to cron expressions (one per distinct time).
func (s Schedule) crons() []string {
	if s.cron != "" {
		return []string{s.cron}
	}
	switch s.Kind {
	case SchedDaily:
		out := make([]string, 0, len(s.Times))
		for _, t := range s.Times {
			h, m, _ := parseHM(t)
			out = append(out, fmt.Sprintf("%d %d * * %s", m, h, s.dowField()))
		}
		return out
	case SchedInterval:
		hours := "*"
		if s.FromHour != 0 || s.ToHour != 0 {
			hours = fmt.Sprintf("%d-%d", s.FromHour, s.ToHour-1)
		}
		if s.EveryMinutes < 60 {
			return []string{fmt.Sprintf("*/%d %s * * %s", s.EveryMinutes, hours, s.dowField())}
		}
		step := s.EveryMinutes / 60
		if hours == "*" {
			hours = "0-23"
		}
		return []string{fmt.Sprintf("0 %s/%d * * %s", hours, step, s.dowField())}
	case SchedMonthly:
		h, m, _ := parseHM(s.Times[0])
		if s.Week != 0 {
			return []string{fmt.Sprintf("%d %d * * %d", m, h, s.Weekday)} // the week is checked by allowed
		}
		return []string{fmt.Sprintf("%d %d %d * *", m, h, s.DayOfMonth)}
	}
	return nil
}

// Next returns the first run time after t, or zero for manual schedules
// (and after the end of the schedule's period).
func (s Schedule) Next(t time.Time) time.Time {
	next := s.nextRaw(t)
	for i := 0; i < 2000 && !next.IsZero(); i++ {
		ok, from := s.allowed(next)
		switch {
		case ok:
			return next
		case from.IsZero():
			return time.Time{} // past the end of the period
		case s.Kind == SchedSinceLast:
			next = from
		default:
			next = s.nextRaw(from.Add(-time.Second))
		}
	}
	return time.Time{}
}

// allowed checks a run time against the months, period and week of the
// schedule. When it is not allowed, from is the earliest time that may be
// (zero: none will).
func (s Schedule) allowed(t time.Time) (ok bool, from time.Time) {
	loc := t.Location()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	if s.ValidUntil != "" {
		if until, err := time.ParseInLocation(time.DateOnly, s.ValidUntil, loc); err == nil && day.After(until) {
			return false, time.Time{}
		}
	}
	if s.ValidFrom != "" {
		if start, err := time.ParseInLocation(time.DateOnly, s.ValidFrom, loc); err == nil && day.Before(start) {
			return false, start
		}
	}
	if len(s.Months) > 0 && !slices.Contains(s.Months, int(t.Month())) {
		first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
		for i := 1; i <= 12; i++ {
			if m := first.AddDate(0, i, 0); slices.Contains(s.Months, int(m.Month())) {
				return false, m
			}
		}
		return false, time.Time{}
	}
	if s.Kind == SchedMonthly && s.Week != 0 {
		last := day.AddDate(0, 1, -day.Day()).Day()
		if in := (s.Week == 5 && t.Day()+7 > last) || (s.Week < 5 && (t.Day()-1)/7+1 == s.Week); !in {
			return false, t.Add(time.Minute)
		}
	}
	if s.Kind == SchedSinceLast && len(s.Days) > 0 && !slices.Contains(s.Days, int(t.Weekday())) {
		return false, day.AddDate(0, 0, 1)
	}
	return true, time.Time{}
}

// minuteWindow returns FromTime and ToTime in minutes of the day.
func (s Schedule) minuteWindow() (from, to int, err error) {
	if s.FromTime == "" || s.ToTime == "" {
		return 0, 0, errors.New("give both from_time and to_time")
	}
	fh, fm, err := parseHM(s.FromTime)
	if err != nil {
		return 0, 0, err
	}
	if strings.TrimSpace(s.ToTime) == "24:00" {
		to = 24 * 60
	} else {
		th, tm, err := parseHM(s.ToTime)
		if err != nil {
			return 0, 0, err
		}
		to = th*60 + tm
	}
	from = fh*60 + fm
	if to <= from {
		return 0, 0, errors.New("the time window must end after it starts")
	}
	return from, to, nil
}

// nextRaw is the next time after t by the times, days and interval of the
// schedule, before months, period and week are applied.
func (s Schedule) nextRaw(t time.Time) time.Time {
	switch {
	case s.cron == "" && s.Kind == SchedSinceLast:
		return t.Add(time.Duration(s.EveryHours) * time.Hour).Truncate(time.Minute)
	case s.cron == "" && s.Kind == SchedInterval && s.FromTime != "":
		from, to, err := s.minuteWindow()
		if err != nil || s.EveryMinutes <= 0 {
			return time.Time{}
		}
		loc := t.Location()
		for d := 0; d <= 8; d++ {
			day := time.Date(t.Year(), t.Month(), t.Day()+d, 0, 0, 0, 0, loc)
			if len(s.Days) > 0 && !slices.Contains(s.Days, int(day.Weekday())) {
				continue
			}
			for m := from; m < to; m += s.EveryMinutes {
				if c := time.Date(day.Year(), day.Month(), day.Day(), m/60, m%60, 0, 0, loc); c.After(t) {
					return c
				}
			}
		}
		return time.Time{}
	}
	var next time.Time
	for _, expr := range s.crons() {
		c, err := cronParser.Parse(expr)
		if err != nil {
			continue
		}
		if n := c.Next(t); next.IsZero() || n.Before(next) {
			next = n
		}
	}
	return next
}

var dayNames = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

func (s Schedule) daysText(l *language) string {
	switch d := s.dowField(); d {
	case "*":
		return l.T("every day")
	case "1,2,3,4,5":
		return l.T("Mon–Fri")
	case "0,6":
		return l.T("Sat and Sun")
	}
	names := make([]string, len(s.Days))
	for i, d := range s.Days {
		names[i] = l.T(dayNames[d])
	}
	return l.T("on %s", strings.Join(names, ", "))
}

// Describe returns a human readable summary, e.g. "Every day at 22:00".
func (s Schedule) Describe() string { return s.DescribeIn(languages[0]) }

// DescribeIn is Describe in a console language.
func (s Schedule) DescribeIn(l *language) string {
	if s.cron != "" {
		return "cron: " + s.cron
	}
	d := s.describeBase(l)
	if len(s.Months) > 0 {
		m := make([]string, len(s.Months))
		for i, n := range s.Months {
			m[i] = strconv.Itoa(n)
		}
		d = l.T("%s; months %s", d, strings.Join(m, ", "))
	}
	if s.ValidFrom != "" {
		d = l.T("%s; from %s", d, s.ValidFrom)
	}
	if s.ValidUntil != "" {
		d = l.T("%s; until %s", d, s.ValidUntil)
	}
	return d
}

// monthlyWeek are the texts of monthly schedules on a weekday, by Week.
var monthlyWeek = []string{"", "Monthly on the first %s at %s", "Monthly on the second %s at %s",
	"Monthly on the third %s at %s", "Monthly on the fourth %s at %s", "Monthly on the last %s at %s"}

func (s Schedule) describeBase(l *language) string {
	switch s.Kind {
	case SchedSinceLast:
		d := l.T("Every %d h after the previous backup", s.EveryHours)
		if len(s.Days) > 0 {
			d += ", " + s.daysText(l)
		}
		return d
	case SchedDaily:
		d := []rune(s.daysText(l))
		return l.T("%s at %s", strings.ToUpper(string(d[:1]))+string(d[1:]), strings.Join(s.Times, ", "))
	case SchedInterval:
		var every string
		switch {
		case s.EveryMinutes < 60:
			every = l.T("Every %d minutes", s.EveryMinutes)
		case s.EveryMinutes == 60:
			every = l.T("Every hour")
		default:
			every = l.T("Every %d hours", s.EveryMinutes/60)
		}
		if s.FromTime != "" {
			every = l.T("%s between %s and %s", every, s.FromTime, s.ToTime)
		} else if s.FromHour != 0 || s.ToHour != 0 {
			every = l.T("%s between %02d:00 and %02d:00", every, s.FromHour, s.ToHour)
		}
		return every + ", " + s.daysText(l)
	case SchedMonthly:
		if s.Week >= 1 && s.Week <= 5 {
			return l.T(monthlyWeek[s.Week], l.T(dayNames[s.Weekday]), s.Times[0])
		}
		return l.T("Monthly on day %d at %s", s.DayOfMonth, s.Times[0])
	case SchedAfter:
		return l.T("After each backup of the source job")
	case SchedChain:
		if s.AfterAny {
			return l.T("After another job finished")
		}
		return l.T("After another job succeeded")
	}
	return l.T("Manual only")
}

// IsManual reports whether the job only runs when started by hand.
func (s Schedule) IsManual() bool { return s.cron == "" && (s.Kind == SchedManual || s.Kind == "") }

// validatePeriod checks and normalizes Months, ValidFrom and ValidUntil.
func (s *Schedule) validatePeriod() error {
	var months []int
	for _, m := range s.Months {
		if m < 1 || m > 12 {
			return fmt.Errorf("invalid month %d (1 to 12)", m)
		}
		if !slices.Contains(months, m) {
			months = append(months, m)
		}
	}
	sort.Ints(months)
	if len(months) == 12 {
		months = nil
	}
	s.Months = months
	var from, until time.Time
	var err error
	if s.ValidFrom != "" {
		if from, err = time.Parse(time.DateOnly, s.ValidFrom); err != nil {
			return fmt.Errorf("invalid valid_from %q (use YYYY-MM-DD)", s.ValidFrom)
		}
	}
	if s.ValidUntil != "" {
		if until, err = time.Parse(time.DateOnly, s.ValidUntil); err != nil {
			return fmt.Errorf("invalid valid_until %q (use YYYY-MM-DD)", s.ValidUntil)
		}
		if !from.IsZero() && until.Before(from) {
			return errors.New("valid_until is before valid_from")
		}
	}
	return nil
}
