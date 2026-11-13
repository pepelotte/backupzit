package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The platform sends wishes BackupZit has no field of the same name for
// under "ext". What BackupZit can do is translated here; what it cannot
// comes back as a warning for the technician.

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)
var hmRe = regexp.MustCompile(`^([01]?\d|2[0-4]):([0-5]\d)$`)

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

// weekOfMonth reads "first" … "fourth", "last" or 1 … 5 (-1 = last).
func weekOfMonth(v any) int {
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "first", "1st", "erster", "ersten":
			return 1
		case "second", "2nd", "zweiter", "zweiten":
			return 2
		case "third", "3rd", "dritter", "dritten":
			return 3
		case "fourth", "4th", "vierter", "vierten":
			return 4
		case "last", "letzter", "letzten":
			return 5
		}
	}
	if n, ok := num(v); ok {
		switch {
		case n == -1:
			return 5
		case n >= 1 && n <= 5 && n == math.Trunc(n):
			return int(n)
		}
	}
	return 0
}

// mapScheduleExt applies schedule.ext to a BackupZit schedule.
func mapScheduleExt(m, ext map[string]any) []string {
	var warn []string
	kind, _ := m["kind"].(string)
	for k, v := range ext {
		switch k {
		case "months":
			list, _ := v.([]any)
			var months []int
			for _, x := range list {
				if n, ok := num(x); ok && n >= 1 && n <= 12 {
					months = append(months, int(n))
				}
			}
			if len(months) > 0 {
				m["months"] = months
			}
		case "month_weekday":
			mw, _ := v.(map[string]any)
			if mw == nil {
				continue
			}
			week := weekOfMonth(mw["nth"])
			day, ok := num(mw["day"])
			if week == 0 || !ok || day < 0 || day > 6 {
				warn = append(warn, "Der Wochentag im Monat ist unvollständig und wurde nicht übernommen")
				continue
			}
			if kind != "monthly" {
				warn = append(warn, "Ein Wochentag im Monat gilt nur für monatliche Zeitpläne")
				continue
			}
			m["week"], m["weekday"] = week, int(day)
			delete(m, "day_of_month")
		case "range":
			r, _ := v.(map[string]any)
			for src, dst := range map[string]string{"from": "valid_from", "to": "valid_until"} {
				if s, _ := r[src].(string); dateRe.MatchString(s) {
					m[dst] = s[:10]
				}
			}
		case "since_last_hours":
			if n, ok := num(v); ok && n > 0 {
				m["kind"] = "since_last"
				m["every_hours"] = int(math.Ceil(n))
				kind = "since_last"
			}
		case "from_minute", "to_minute":
			// Handled together below.
		case "on_boot":
			if truthy(v) {
				warn = append(warn, "„Beim Systemstart“ kann BackupZit nicht: Sicherungen starten nach Zeitplan; ist der Rechner aus, läuft die fällige Sicherung, sobald er wieder online ist")
			}
		case "catch_up":
			if b, ok := v.(bool); ok && !b {
				warn = append(warn, "BackupZit holt verpasste Sicherungen immer nach, sobald der Rechner wieder online ist (abschaltbar nur über ein Sicherungsfenster)")
			}
		case "prevent_sleep":
			if truthy(v) {
				warn = append(warn, "Den Ruhezustand während der Sicherung verhindern kann BackupZit nicht")
			}
		default:
			if v != nil && v != false && v != "" {
				warn = append(warn, fmt.Sprintf("Zeitplan-Option %q kennt BackupZit nicht", k))
			}
		}
	}
	from, _ := ext["from_minute"].(string)
	to, _ := ext["to_minute"].(string)
	if from != "" || to != "" {
		switch {
		case kind != "interval":
			warn = append(warn, "Ein Zeitfenster mit Minuten gilt nur für wiederholte Sicherungen (alle x Minuten/Stunden)")
		case hmRe.MatchString(from) && hmRe.MatchString(to):
			m["from_time"], m["to_time"] = from, to
			delete(m, "from_hour")
			delete(m, "to_hour")
		default:
			warn = append(warn, "Das Zeitfenster mit Minuten ist unvollständig (HH:MM bis HH:MM) und wurde nicht übernommen")
		}
	}
	return warn
}

// runsPerHour estimates how often a BackupZit schedule runs (0 = unknown).
func runsPerHour(s map[string]any) float64 {
	if s == nil {
		return 0
	}
	dayShare := 1.0
	if d, _ := s["days"].([]any); len(d) > 0 {
		dayShare = float64(len(d)) / 7
	} else if d, _ := s["days"].([]int); len(d) > 0 {
		dayShare = float64(len(d)) / 7
	}
	monthShare := 1.0
	if m, _ := s["months"].([]int); len(m) > 0 {
		monthShare = float64(len(m)) / 12
	}
	switch s["kind"] {
	case "daily":
		t, _ := s["times"].([]any)
		return float64(max(len(t), 1)) * dayShare * monthShare / 24
	case "interval":
		every, _ := num(s["every_minutes"])
		if every <= 0 {
			return 0
		}
		perDay := 1440 / every
		if f, _ := s["from_time"].(string); f != "" {
			t, _ := s["to_time"].(string)
			if a, b := minutesOf(f), minutesOf(t); b > a {
				perDay = math.Ceil(float64(b-a) / every)
			}
		} else if f, ok := num(s["from_hour"]); ok {
			if t, ok := num(s["to_hour"]); ok && t > f {
				if every < 60 {
					perDay = (t - f) * 60 / every
				} else {
					perDay = math.Ceil((t - f) / (every / 60))
				}
			}
		}
		return perDay * dayShare * monthShare / 24
	case "since_last":
		if h, _ := num(s["every_hours"]); h > 0 {
			return 1 / h
		}
	case "monthly":
		return monthShare / (30 * 24)
	}
	return 0
}

func minutesOf(hm string) int {
	p := hmRe.FindStringSubmatch(hm)
	if p == nil {
		return -1
	}
	h, _ := strconv.Atoi(p[1])
	m, _ := strconv.Atoi(p[2])
	return h*60 + m
}

// mapRetentionExt turns an age-based retention into the counts BackupZit
// keeps. schedule (may be nil) helps for ages in hours.
func mapRetentionExt(m, ext, schedule map[string]any) []string {
	var warn []string
	if w, _ := ext["when"].(string); w == "before" {
		warn = append(warn, "BackupZit bereinigt alte Sicherungen immer nach der neuen Sicherung")
	}
	if mode, _ := ext["mode"].(string); mode != "age" {
		return warn
	}
	age, ok := num(ext["max_age"])
	if !ok || age <= 0 {
		return append(warn, "Die Aufbewahrung nach Alter hat keine Dauer und wurde nicht übernommen")
	}
	n := int(math.Ceil(age))
	unit, _ := ext["unit"].(string)
	set := func(field string, v int, text string) {
		for _, k := range []string{"keep_last", "keep_daily", "keep_weekly", "keep_monthly"} {
			delete(m, k)
		}
		m[field] = min(v, 999)
		warn = append(warn, text)
	}
	switch unit {
	case "hours":
		if r := runsPerHour(schedule); r > 0 {
			k := max(int(math.Ceil(age*r)), 1)
			set("keep_last", k, fmt.Sprintf("Aufbewahrung %d Stunden umgesetzt als die letzten %d Sicherungen (nach dem Zeitplan)", n, k))
		} else {
			d := int(math.Ceil(age / 24))
			set("keep_daily", d, fmt.Sprintf("Aufbewahrung %d Stunden umgesetzt als %d tägliche Sicherungen", n, d))
		}
	case "days":
		set("keep_daily", n, fmt.Sprintf("Aufbewahrung %d Tage umgesetzt als %d tägliche Sicherungen: je Tag mit Sicherung bleibt die neueste", n, n))
	case "weeks":
		set("keep_weekly", n, fmt.Sprintf("Aufbewahrung %d Wochen umgesetzt als %d wöchentliche Sicherungen", n, n))
	case "months":
		set("keep_monthly", n, fmt.Sprintf("Aufbewahrung %d Monate umgesetzt als %d monatliche Sicherungen", n, n))
	case "years":
		set("keep_monthly", n*12, fmt.Sprintf("Aufbewahrung %d Jahre umgesetzt als %d monatliche Sicherungen", n, min(n*12, 999)))
	default:
		warn = append(warn, fmt.Sprintf("Unbekannte Einheit %q der Aufbewahrung nach Alter; es gelten die Anzahlen", unit))
	}
	return warn
}

// hourRange returns the hours (0..23) as one range from-to (to exclusive,
// may wrap past midnight); ok is false when they are not contiguous.
func hourRange(hours []bool) (from, to int, ok bool) {
	n := 0
	for _, h := range hours {
		if h {
			n++
		}
	}
	if n == 0 || n == 24 {
		return 0, 0, n == 24
	}
	for start := 0; start < 24; start++ {
		if hours[start] && !hours[(start+23)%24] {
			for i := 0; i < n; i++ {
				if !hours[(start+i)%24] {
					return 0, 0, false
				}
			}
			return start, (start + n) % 24, true
		}
	}
	return 0, 0, false
}

// mapOptionsExt applies the performance window: hours without backup
// become the backup window, throttled hours the speed limit.
func mapOptionsExt(m, ext map[string]any) []string {
	var warn []string
	if truthy(ext["physical_seeding"]) {
		warn = append(warn, "Erst-Seeding per USB-Platte kann BackupZit nicht: die erste Sicherung lädt alles hoch, danach nur Änderungen")
	}
	pw, _ := ext["performance_window"].(map[string]any)
	if pw == nil {
		return warn
	}
	if c, ok := num(nested(pw, "normal", "cpu")); ok && c > 0 && c < 100 {
		warn = append(warn, "Eine CPU-Begrenzung kann BackupZit nicht einstellen")
	} else if c, ok := num(nested(pw, "throttled", "cpu")); ok && c > 0 && c < 100 {
		warn = append(warn, "Eine CPU-Begrenzung kann BackupZit nicht einstellen")
	}
	if s, ok := num(nested(pw, "normal", "speedPercent")); ok && s > 0 && s < 100 {
		warn = append(warn, "Eine prozentuale Geschwindigkeit kann BackupZit nicht einstellen, nur eine feste Grenze in MB/s")
	}
	grid, _ := pw["grid"].([]any)
	if len(grid) != 7 {
		return warn
	}
	var rows []string
	for _, g := range grid {
		s, _ := g.(string)
		if len(s) != 24 {
			return append(warn, "Das Leistungsfenster ist unvollständig (7 × 24 Stunden) und wurde nicht übernommen")
		}
		rows = append(rows, strings.ToLower(s))
	}
	sort.Strings(rows)
	if rows[0] != rows[6] {
		return append(warn, "BackupZit kennt nur ein Fenster für alle Wochentage; das Leistungsfenster mit unterschiedlichen Tagen wurde nicht übernommen")
	}
	row := rows[0]
	allowed, throttled := make([]bool, 24), make([]bool, 24)
	for i, c := range row {
		allowed[i] = c != 'x'
		throttled[i] = c == 't'
	}
	if _, set := m["window"]; !set {
		switch from, to, ok := hourRange(allowed); {
		case !ok && !slicesAny(allowed):
			warn = append(warn, "Im Leistungsfenster ist keine Stunde für Sicherungen frei; es wurde nicht übernommen")
		case !ok:
			warn = append(warn, "BackupZit kennt nur ein zusammenhängendes Sicherungsfenster; die gesperrten Stunden wurden nicht übernommen")
		case from != to:
			m["window"], m["window_from"], m["window_to"] = true, from, to
			warn = append(warn, fmt.Sprintf("Gesperrte Stunden umgesetzt als Sicherungsfenster %02d–%02d Uhr: Sicherungen starten nur darin, eine laufende wird nicht angehalten", from, to))
		}
	}
	if _, set := m["limit_mbps"]; !set && slicesAny(throttled) {
		kbps, _ := num(nested(pw, "throttled", "kbps"))
		from, to, ok := hourRange(throttled)
		switch {
		case !ok:
			warn = append(warn, "BackupZit kennt nur einen zusammenhängenden Zeitraum für die Bandbreitengrenze; die gedrosselten Stunden wurden nicht übernommen")
		case kbps <= 0:
			warn = append(warn, "Gedrosselte Stunden ohne Bandbreite (kbps) wurden nicht übernommen")
		default:
			mbs := int(math.Round(kbps / 8000)) // kbit/s → MB/s
			if mbs < 1 {
				mbs = 1
				warn = append(warn, fmt.Sprintf("BackupZit begrenzt in ganzen MB/s; %.0f kbit/s wurden auf 1 MB/s (8000 kbit/s) aufgerundet", kbps))
			}
			m["limit_mbps"], m["limit_from"], m["limit_to"] = mbs, from, to
		}
	}
	return warn
}

func slicesAny(b []bool) bool {
	for _, x := range b {
		if x {
			return true
		}
	}
	return false
}

func nested(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}
