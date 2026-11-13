package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// cloud adds the customer's provisioned storage to the fake console.
func (f *fakes) cloud() {
	f.mu.Lock()
	f.targets = append(f.targets, map[string]any{"id": 3, "name": "Müller GmbH", "kind": "s3",
		"location": "s3://s3.eu-central-003.backblazeb2.com/itdocs-bz-backups/c-" + customer})
	f.mu.Unlock()
}

func TestJobExtMapping(t *testing.T) {
	f, c := newFakes(t)
	id := f.enqueue("job.create", map[string]any{
		"name": "PC-01", "kind": "files", "agent_id": 12, "target_id": 3, "paths": []string{`C:\Daten`},
		"schedule": map[string]any{"kind": "monthly", "day_of_month": 1, "times": []string{"23:00"}, "ext": map[string]any{
			"months": []int{3, 6, 9, 12}, "month_weekday": map[string]any{"nth": "last", "day": 5},
			"range": map[string]any{"from": "2026-12-01T00:00:00Z", "to": "2027-12-31"}, "on_boot": true, "catch_up": true, "prevent_sleep": true}},
		"retention": map[string]any{"keep_last": 3, "keep_daily": 7, "ext": map[string]any{"mode": "age", "max_age": 90, "unit": "days", "when": "before"}},
		"options": map[string]any{"ext": map[string]any{"physical_seeding": true, "performance_window": map[string]any{
			"grid":      []string{"xxxxxxnnnnnnnnnnnnttttxx", "xxxxxxnnnnnnnnnnnnttttxx", "xxxxxxnnnnnnnnnnnnttttxx", "xxxxxxnnnnnnnnnnnnttttxx", "xxxxxxnnnnnnnnnnnnttttxx", "xxxxxxnnnnnnnnnnnnttttxx", "xxxxxxnnnnnnnnnnnnttttxx"},
			"normal":    map[string]any{"cpu": 100, "speedPercent": 100},
			"throttled": map[string]any{"cpu": 50, "kbps": 40000}}}},
	})
	process(t, c)
	res := f.result(id)
	if res["status"] != "done" {
		t.Fatalf("create: %v", res)
	}
	body := f.jobPosts[0]
	s := body["schedule"].(map[string]any)
	if s["week"] != float64(5) || s["weekday"] != float64(5) || s["day_of_month"] != nil || fmt.Sprint(s["months"]) != "[3 6 9 12]" ||
		s["valid_from"] != "2026-12-01" || s["valid_until"] != "2027-12-31" {
		t.Errorf("schedule: %v", s)
	}
	if r := body["retention"].(map[string]any); r["keep_daily"] != float64(90) || r["keep_last"] != nil || len(r) != 1 {
		t.Errorf("retention: %v", r)
	}
	o := body["options"].(map[string]any)
	if o["window"] != true || o["window_from"] != float64(6) || o["window_to"] != float64(22) ||
		o["limit_mbps"] != float64(5) || o["limit_from"] != float64(18) || o["limit_to"] != float64(22) {
		t.Errorf("options: %v", o)
	}
	w := fmt.Sprint(res["result"].(map[string]any)["warnings"])
	for _, want := range []string{"Systemstart", "Ruhezustand", "nach der neuen Sicherung", "90 tägliche", "Seeding", "CPU", "06–22"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q missing: %s", want, w)
		}
	}
	if strings.Contains(w, "nachholen") || strings.Contains(w, "holt verpasste") {
		t.Errorf("catch_up true warned: %s", w)
	}

	// An interval window with minutes, and since_last.
	id = f.enqueue("job.update", map[string]any{"job_id": 9, "schedule": map[string]any{"kind": "interval", "every_minutes": 60, "from_hour": 8, "to_hour": 18,
		"ext": map[string]any{"from_minute": "08:30", "to_minute": "17:45", "catch_up": false}},
		"retention": map[string]any{"ext": map[string]any{"mode": "age", "max_age": 48, "unit": "hours"}}})
	process(t, c)
	p := f.jobPatches[0]
	if s := p["schedule"].(map[string]any); s["from_time"] != "08:30" || s["to_time"] != "17:45" || s["from_hour"] != nil {
		t.Errorf("interval: %v", s)
	}
	// 10 runs a day (08:30 … 17:30) over 48 hours.
	if r := p["retention"].(map[string]any); r["keep_last"] != float64(20) {
		t.Errorf("hours: %v", r)
	}
	if w := fmt.Sprint(f.result(id)["result"].(map[string]any)["warnings"]); !strings.Contains(w, "holt verpasste") {
		t.Errorf("catch_up false: %s", w)
	}
	id = f.enqueue("job.update", map[string]any{"job_id": 9, "schedule": map[string]any{"kind": "daily", "times": []string{"22:00"},
		"ext": map[string]any{"since_last_hours": 36, "from_minute": "08:00", "to_minute": "09:00"}}, "ext_disks": []any{map[string]any{"disk": 0}},
		"destination": map[string]any{"type": "local", "path": `E:\B`}})
	process(t, c)
	p = f.jobPatches[1]
	if s := p["schedule"].(map[string]any); s["kind"] != "since_last" || s["every_hours"] != float64(36) {
		t.Errorf("since_last: %v", s)
	}
	if w := fmt.Sprint(f.result(id)["result"].(map[string]any)["warnings"]); !strings.Contains(w, "Speicherziel") || !strings.Contains(w, "Datenträgerauswahl") ||
		!strings.Contains(w, "nur für wiederholte") {
		t.Errorf("update warnings: %s", w)
	}
}

func TestHelpers(t *testing.T) {
	h := func(s string) []bool {
		b := make([]bool, 24)
		for i, c := range s {
			b[i] = c == '1'
		}
		return b
	}
	for in, want := range map[string]string{
		"000000111111111111000000": "6 18 true",
		"111111000000000000111111": "18 6 true",
		"111111111111111111111111": "0 0 true",
		"000000000000000000000000": "0 0 false",
		"110000000000000000000011": "22 2 true",
		"101000000000000000000000": "0 0 false",
	} {
		if f, to, ok := hourRange(h(in)); fmt.Sprint(f, to, ok) != want {
			t.Errorf("%s: %d %d %v, want %s", in, f, to, ok, want)
		}
	}
	for v, want := range map[any]int{"first": 1, "last": 5, float64(-1): 5, float64(3): 3, "fifth": 0, float64(2.5): 0} {
		if got := weekOfMonth(v); got != want {
			t.Errorf("week %v: %d", v, got)
		}
	}
	var sched map[string]any
	_ = json.Unmarshal([]byte(`{"kind":"daily","times":["08:00","20:00"],"days":[1,2,3,4,5]}`), &sched)
	if r := runsPerHour(sched); r < 0.059 || r > 0.06 { // 2 × 5/7 / 24
		t.Errorf("runs per hour: %v", r)
	}
	m := map[string]any{"keep_last": 5}
	if w := mapRetentionExt(m, map[string]any{"mode": "age", "max_age": 2, "unit": "years"}, nil); m["keep_monthly"] != 24 || m["keep_last"] != nil || len(w) != 1 {
		t.Errorf("years: %v %v", m, w)
	}
	m = map[string]any{}
	if w := mapRetentionExt(m, map[string]any{"mode": "age", "max_age": 36, "unit": "hours"}, nil); m["keep_daily"] != 2 || len(w) != 1 {
		t.Errorf("hours without schedule: %v %v", m, w)
	}
	// Different days cannot become one window.
	o := map[string]any{}
	grid := []any{"xxxxxxxxxxxxxxxxxxxxxxxx", "nnnnnnnnnnnnnnnnnnnnnnnn", "nnnnnnnnnnnnnnnnnnnnnnnn", "nnnnnnnnnnnnnnnnnnnnnnnn", "nnnnnnnnnnnnnnnnnnnnnnnn", "nnnnnnnnnnnnnnnnnnnnnnnn", "nnnnnnnnnnnnnnnnnnnnnnnn"}
	if w := mapOptionsExt(o, map[string]any{"performance_window": map[string]any{"grid": grid}}); len(o) != 0 || !strings.Contains(fmt.Sprint(w), "Wochentage") {
		t.Errorf("per-day grid: %v %v", o, w)
	}
	// A small throttle is rounded up to 1 MB/s.
	o = map[string]any{}
	row := "nnnnnnnnttttttttttnnnnnn"
	grid = []any{row, row, row, row, row, row, row}
	if w := mapOptionsExt(o, map[string]any{"performance_window": map[string]any{"grid": grid, "throttled": map[string]any{"kbps": 2000}}}); o["limit_mbps"] != 1 ||
		o["limit_from"] != 8 || o["limit_to"] != 18 || o["window"] != nil || !strings.Contains(fmt.Sprint(w), "aufgerundet") {
		t.Errorf("throttle: %v %v", o, w)
	}
	if h, s, d, ok := smbParts(`\\nas01\backup\pc\01`); !ok || h != "nas01" || s != "backup" || d != "pc/01" {
		t.Errorf("unc: %s %s %s %v", h, s, d, ok)
	}
	if _, _, _, ok := smbParts(`C:\x`); ok {
		t.Error("local path taken as share")
	}
	if b, err := recoveryJSON(fakeCode); err != nil || !strings.Contains(string(b), `"token": "tok-123"`) || !strings.Contains(string(b), `"server": "https://backup.example:8443"`) {
		t.Errorf("recovery.json: %s %v", b, err)
	}
	if _, err := recoveryJSON("BZ1-!!"); err == nil {
		t.Error("broken code accepted")
	}
}

func TestDestinations(t *testing.T) {
	f, c := newFakes(t)
	f.cloud()
	id := f.enqueue("job.create", map[string]any{"name": "NAS-Sicherung", "kind": "files", "agent_id": 12, "paths": []string{`C:\Daten`},
		"destination": map[string]any{"type": "network", "path": `\\nas01\backup\pc`, "username": `FIRMA\sicherung`, "password": "geheim"}})
	process(t, c)
	res := f.result(id)
	if res["status"] != "done" {
		t.Fatalf("network: %v", res)
	}
	tp := f.targetPosts[0]
	if tp["kind"] != "smb" || tp["smb_host"] != "nas01" || tp["smb_share"] != "backup" || tp["smb_path"] != "pc" || tp["smb_user"] != "sicherung" ||
		tp["smb_domain"] != "FIRMA" || tp["smb_password"] != "geheim" || tp["recovery_key_from"] != float64(3) || tp["name"] != `Müller GmbH · NAS \\nas01\backup\pc` {
		t.Errorf("smb target: %v", tp)
	}
	r := res["result"].(map[string]any)
	dt := r["destination_target"].(map[string]any)
	if f.jobPosts[0]["target_id"] != dt["id"] || r["destination_created"] != true || strings.Contains(fmt.Sprint(r), "geheim") ||
		!strings.Contains(fmt.Sprint(r["warnings"]), "erreicht die Freigabe nicht") {
		t.Errorf("result: %v", r)
	}
	// The same share again: reused, with the password given now.
	id = f.enqueue("job.create", map[string]any{"name": "NAS 2", "kind": "files", "agent_id": 12,
		"destination": map[string]any{"type": "network", "path": `//nas01/backup/pc`, "username": `FIRMA\sicherung`, "password": "neu"}})
	process(t, c)
	if r := f.result(id)["result"].(map[string]any); r["destination_created"] != false || len(f.targetPosts) != 1 ||
		len(f.targetPatches) != 1 || f.targetPatches[0]["smb_password"] != "neu" || f.targetPatches[0]["_path"] != fmt.Sprintf("/targets/%v", dt["id"]) {
		t.Errorf("reuse: %v %v", r, f.targetPatches)
	}
	// A local disk of the agent.
	id = f.enqueue("job.create", map[string]any{"name": "USB", "kind": "files", "agent_id": 12, "destination": map[string]any{"type": "local", "path": `E:\Backups`}})
	process(t, c)
	if res := f.result(id); res["status"] != "done" || f.targetPosts[1]["kind"] != "local" || f.targetPosts[1]["url"] != `E:\Backups` ||
		f.targetPosts[1]["recovery_key_from"] != float64(3) {
		t.Errorf("local: %v %v", res, f.targetPosts[1])
	}
	for _, d := range []map[string]any{
		{"type": "local", "path": `Backups`},
		{"type": "network", "path": `\\nas01`, "username": "u", "password": "p"},
		{"type": "network", "path": `\\nas01\b`, "username": "u"},
	} {
		id = f.enqueue("job.create", map[string]any{"name": "x", "kind": "files", "agent_id": 12, "destination": d})
		process(t, c)
		if res := f.result(id); res["status"] != "failed" {
			t.Errorf("%v accepted: %v", d, res)
		}
	}
	// target.update passes only name and credentials.
	id = f.enqueue("target.update", map[string]any{"target_id": 4, "smb_password": "x", "s3_bucket": "other"})
	process(t, c)
	if res := f.result(id); res["status"] != "done" || f.targetPatches[1]["s3_bucket"] != nil || f.targetPatches[1]["smb_password"] != "x" {
		t.Errorf("target.update: %v %v", res, f.targetPatches)
	}
}

func TestExtDisks(t *testing.T) {
	f, c := newFakes(t)
	for i, tc := range []struct {
		sel  []any
		want string
		warn bool
	}{
		{[]any{map[string]any{"disk": 1, "partitions": []int{1}}}, "1 [1]", false},
		{[]any{map[string]any{"disk": 0, "whole": true}}, "0 <nil>", false},
		{[]any{map[string]any{"disk": 0, "whole": true}, map[string]any{"disk": 1, "whole": true}}, "-1 <nil>", false},
		{[]any{map[string]any{"disk": 0, "whole": true}, map[string]any{"disk": 1, "partitions": []int{1}}}, "-1 <nil>", true},
		{[]any{map[string]any{"disk": 0, "whole": true}, map[string]any{"disk": 2, "whole": true}}, "-1 <nil>", true},
	} {
		id := f.enqueue("job.create", map[string]any{"name": "img", "kind": "image", "agent_id": 12, "target_id": 3, "image_disk": -1, "ext_disks": tc.sel})
		process(t, c)
		res := f.result(id)
		b := f.jobPosts[i]
		if got := fmt.Sprint(b["image_disk"], " ", b["image_partitions"]); got != tc.want {
			t.Errorf("%d: %s, want %s", i, got, tc.want)
		}
		if w := res["result"].(map[string]any)["warnings"] != nil; w != tc.warn {
			t.Errorf("%d: warnings %v", i, res["result"])
		}
	}
}

func TestRestoreAndRecovery(t *testing.T) {
	f, c := newFakes(t)
	id := f.enqueue("restore.files_original", map[string]any{"run_id": 77, "agent_id": 12, "paths": []string{"C/Users/ana"}, "password": "x"})
	process(t, c)
	if res := f.result(id); res["status"] != "done" || f.restores[0]["original"] != true || f.restores[0]["_path"] != "/runs/77/restore" || f.restores[0]["password"] != nil {
		t.Errorf("original: %v %v", res, f.restores)
	}
	// A machine with two disks: one restore per disk.
	id = f.enqueue("restore.machine", map[string]any{"run_id": 80, "agent_id": 15, "target_type": "physical", "new_hardware": true,
		"mapping": []any{map[string]any{"source_disk": 0, "target_disk": 0}, map[string]any{"source_disk": 1, "target_disk": 2}}})
	process(t, c)
	res := f.result(id)
	r, _ := res["result"].(map[string]any)
	if res["status"] != "done" || fmt.Sprint(r["run_ids"]) != "[102 103]" || len(f.restores) != 3 {
		t.Fatalf("machine: %v", res)
	}
	if d := f.restores[2]; d["_path"] != "/runs/80/restore-disk" || d["disk"] != float64(1) || d["target_disk"] != float64(2) ||
		d["confirm_erase"] != true || d["new_hardware"] != true || d["agent_id"] != float64(15) {
		t.Errorf("disk body: %v", d)
	}
	// Volumes: all volumes of a disk work, some do not.
	id = f.enqueue("restore.volumes", map[string]any{"run_id": 80, "mapping": []any{map[string]any{"source_disk": 0, "target_disk": 1, "partitions": []int{2, 3}}}})
	process(t, c)
	if res := f.result(id); res["status"] != "done" {
		t.Errorf("all volumes: %v", res)
	}
	n := len(f.restores)
	id = f.enqueue("restore.volumes", map[string]any{"run_id": 80, "mapping": []any{map[string]any{"source_disk": 0, "target_disk": 1, "partitions": []int{3}}}})
	process(t, c)
	if res := f.result(id); res["status"] != "failed" || !strings.Contains(fmt.Sprint(res["error"]), "Einzelne Volumes") || len(f.restores) != n {
		t.Errorf("one volume: %v", res)
	}
	for params, want := range map[string]string{
		`{"run_id":81,"mapping":[{"target_disk":0}]}`:                        "Zielgerät",
		`{"run_id":82,"mapping":[{"target_disk":0}]}`:                        "keine Abbild",
		`{"run_id":80,"target_type":"hyperv","mapping":[{"target_disk":0}]}`: "virtuelle Maschine",
		`{"run_id":80}`: "Zuordnung",
		`{"run_id":80,"mapping":[{"source_disk":0}]}`: "Zielplatte",
	} {
		id = f.enqueue("restore.machine", json.RawMessage(params))
		process(t, c)
		if res := f.result(id); res["status"] != "failed" || !strings.Contains(fmt.Sprint(res["error"]), want) {
			t.Errorf("%s: %v", params, res)
		}
	}
	id = f.enqueue("restore.machine", map[string]any{"run_id": 81, "mapping": []any{map[string]any{"target_device": "/dev/sdb"}}})
	process(t, c)
	if res := f.result(id); res["status"] != "done" || f.restores[len(f.restores)-1]["device"] != "/dev/sdb" {
		t.Errorf("linux: %v", res)
	}

	// Recovery stick: recovery.json for 24 hours, revocable.
	f.mu.Lock()
	f.queue = append(f.queue, Command{ID: "rc1", CustomerID: customer, DeviceID: "dev-9", Action: "recovery.code", Params: json.RawMessage(`{}`)})
	f.mu.Unlock()
	process(t, c)
	res = f.result("rc1")
	r, _ = res["result"].(map[string]any)
	cr := f.codeRequests[len(f.codeRequests)-1]
	if res["status"] != "done" || r["storage_path"] != "rc1/recovery.json" || r["enroll_label"] != "itdocs:"+customer+":recovery:dev-9" ||
		cr["ttl_minutes"] != float64(1440) || cr["single_use"] != false || strings.Contains(fmt.Sprint(r), "tok-123") {
		t.Fatalf("recovery.code: %v %v", res, cr)
	}
	var rec map[string]string
	if err := json.Unmarshal(f.uploads["rc1/recovery.json"], &rec); err != nil || rec["token"] != "tok-123" || rec["fingerprint"] != "SHA256:abc" {
		t.Errorf("recovery.json: %s", f.uploads["rc1/recovery.json"])
	}
	id = f.enqueue("recovery.code", map[string]any{"ttl_hours": 1000})
	process(t, c)
	if res := f.result(id); res["status"] != "failed" {
		t.Errorf("long code: %v", res)
	}
	id = f.enqueue("recovery.revoke", map[string]any{})
	process(t, c)
	if res := f.result(id); res["status"] != "done" || f.revokes[0]["label_prefix"] != "itdocs:"+customer+":recovery:" {
		t.Errorf("revoke: %v %v", res, f.revokes)
	}
}

func TestBrowse(t *testing.T) {
	f, c := newFakes(t)
	id := f.enqueue("agent.browse", map[string]any{"agent_id": 12, "kind": "local", "path": ""})
	process(t, c)
	res := f.result(id)
	r, _ := res["result"].(map[string]any)
	b, _ := json.Marshal(r["entries"])
	if res["status"] != "done" || !strings.Contains(string(b), `"name":"C:","path":"C:\\"`) || !strings.Contains(string(b), `"label":"Daten"`) ||
		!strings.Contains(string(b), `"removable":true`) || strings.Count(string(b), `"dir":true`) != 3 {
		t.Errorf("drives: %v %s", res, b)
	}
	for p, want := range map[string]string{`{"agent_id":12,"kind":"local","path":"C:\\Users"}`: "Laufwerke", `{"agent_id":12,"kind":"network","path":""}`: "Netzwerkfreigaben"} {
		id = f.enqueue("agent.browse", json.RawMessage(p))
		process(t, c)
		if res := f.result(id); res["status"] != "failed" || !strings.Contains(fmt.Sprint(res["error"]), want) {
			t.Errorf("%s: %v", p, res)
		}
	}
}
