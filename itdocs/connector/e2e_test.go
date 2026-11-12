//go:build e2e

package main

// End-to-end test against a real BackupZit console (with the itdocs API),
// a fake platform and a fake Backblaze:
//
//	BZC_E2E_URL=https://127.0.0.1:8443 BZC_E2E_TOKEN=bzt_… BZC_E2E_FP=SHA256:… \
//	BZC_E2E_AGENT=1 BZC_E2E_TARGET=4 BZC_E2E_PATH=/tmp/kundendaten go test -tags e2e -run E2E -v

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestE2E(t *testing.T) {
	f, c := newFakes(t)
	c.bz = newBackupZit(os.Getenv("BZC_E2E_URL"), os.Getenv("BZC_E2E_TOKEN"), os.Getenv("BZC_E2E_FP"))
	agent, _ := strconv.Atoi(os.Getenv("BZC_E2E_AGENT"))
	target, _ := strconv.Atoi(os.Getenv("BZC_E2E_TARGET"))
	path := os.Getenv("BZC_E2E_PATH")
	ctx := context.Background()
	step := func(name, action string, params any) map[string]any {
		t.Helper()
		id := f.enqueue(action, params)
		if err := c.processCommands(ctx); err != nil {
			t.Fatal(err)
		}
		res := f.result(id)
		t.Logf("%-18s %-8v %v %v", name, res["status"], res["error"], res["result"])
		return res
	}

	// 1. Customer storage: B2 is faked, the target is real.
	res := step("provision", "target.provision", map[string]string{"name": "E2E Kunde"})
	if res["status"] != "done" {
		t.Fatalf("provision: %v", res)
	}
	r := res["result"].(map[string]any)
	if len(fmt.Sprint(r["recovery_key"])) != 39 || !strings.Contains(fmt.Sprint(r["target"]), "itdocs-bz-backups/c-"+customer) {
		t.Errorf("provision result: %v", r)
	}
	if res := step("provision again", "target.provision", map[string]string{"name": "E2E Kunde"}); res["status"] != "failed" {
		t.Errorf("second provision: %v", res)
	}

	// 2. Enrollment code.
	if res := step("enroll code", "agent.enroll_code", map[string]any{}); !strings.HasPrefix(fmt.Sprint(res["result"].(map[string]any)["code"]), "BZ1-") {
		t.Errorf("enroll code: %v", res)
	}

	// 3. A job with platform extras, run it, wait.
	res = step("job.create", "job.create", map[string]any{
		"name": "E2E Dokumente", "kind": "files", "agent_id": agent, "target_id": target, "paths": []string{path},
		"encryption_password": "x", "schedule": map[string]any{"kind": "daily", "days": []int{1, 2, 3, 4, 5}, "times": []string{"22:00"},
			"ext": map[string]any{"catch_up": true}},
		"retention": map[string]any{"keep_daily": 14}, "options": map[string]any{"compression": "fast", "ext": map[string]any{}}})
	if res["status"] != "done" {
		t.Fatalf("job.create: %v", res)
	}
	job := int64(res["result"].(map[string]any)["id"].(float64))
	res = step("job.run", "job.run", map[string]any{"job_id": job})
	run := int64(res["result"].(map[string]any)["run_id"].(float64))
	var status string
	for i := 0; i < 60 && status != "success" && status != "failed"; i++ {
		time.Sleep(2 * time.Second)
		var x map[string]any
		_ = c.bz.call(ctx, "GET", fmt.Sprintf("/runs/%d", run), nil, &x)
		status = fmt.Sprint(x["status"])
	}
	if status != "success" {
		t.Fatalf("run %d: %s", run, status)
	}

	// 4. Files.
	comps := strings.TrimPrefix(path, "/")
	res = step("files.list", "files.list", map[string]any{"run_id": run, "path": comps})
	if !strings.Contains(fmt.Sprint(res["result"]), "readme.txt") {
		t.Errorf("files.list: %v", res)
	}
	id := f.enqueue("files.download", map[string]any{"run_id": run, "paths": []string{comps + "/readme.txt"}})
	_ = c.processCommands(ctx)
	if res := f.result(id); res["status"] != "done" || string(f.uploads[id+"/readme.txt"]) != "Hallo\n" {
		t.Errorf("files.download: %v %q", res, f.uploads)
	}

	// 5. Change, state, delete.
	res = step("job.update", "job.update", map[string]any{"job_id": job, "name": "E2E Dokumente (2h)",
		"schedule": map[string]any{"kind": "interval", "every_minutes": 120, "from_hour": 7, "to_hour": 19}})
	if !strings.Contains(fmt.Sprint(res["result"]), "Every 2 hours") {
		t.Errorf("job.update: %v", res)
	}
	c.syncStatus(ctx)
	st := f.statuses[len(f.statuses)-1]
	if !strings.Contains(fmt.Sprint(st["jobs"]), "E2E Dokumente (2h)") || !strings.Contains(fmt.Sprint(st["runs"]), fmt.Sprint(run)) ||
		!strings.Contains(fmt.Sprint(st["targets"]), "used_bytes") {
		t.Errorf("status: %v", st)
	}
	if res := step("job.delete", "job.delete", map[string]any{"job_id": job}); res["status"] != "done" {
		t.Errorf("job.delete: %v", res)
	}
}
