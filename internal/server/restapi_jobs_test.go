package server_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/testutil"
)

// apiSend calls the REST API with any method and a JSON body.
func apiSend(t *testing.T, e *env, tok, method, p, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+p, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestAPIJobs creates, changes and deletes jobs over the REST API.
func TestAPIJobs(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	adm := apiToken(t, admin, "admin")
	op := apiToken(t, admin, "operator")
	help := apiToken(t, admin, "restore")
	t1, _ := e.store.CreateTarget(ctx, server.Target{Name: "nas", Kind: "local", URL: filepath.Join(t.TempDir(), "nas")})
	t2, _ := e.store.CreateTarget(ctx, server.Target{Name: "offsite", Kind: "local", URL: filepath.Join(t.TempDir(), "off")})
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	ag := agents[0].ID
	inv := `[{"number":0,"size":68719476736,"sector_size":512,"style":"gpt","system":true,
	  "partitions":[{"number":1,"offset":1048576,"length":209715200,"gpt_type":"C12A7328-F81F-11D2-BA4B-00A0C93EC93B"},
	                {"number":3,"offset":227540992,"length":67554508800,"mount_points":["C:\\"],"file_system":"NTFS"}]}]`
	if err := e.store.TouchAgent(ctx, ag, api.PollRequest{Disks: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}
	type jobOut struct {
		ID              int64           `json:"id"`
		Name            string          `json:"name"`
		Kind            string          `json:"kind"`
		AgentID         int64           `json:"agent_id"`
		Paths           []string        `json:"paths"`
		Excludes        []string        `json:"excludes"`
		ImageDisk       *int            `json:"image_disk"`
		ImagePartitions []int           `json:"image_partitions"`
		Schedule        string          `json:"schedule"`
		ScheduleSpec    server.Schedule `json:"schedule_spec"`
		Retention       struct {
			KeepDaily  int `json:"keep_daily"`
			KeepWeekly int `json:"keep_weekly"`
		} `json:"retention"`
		Options struct {
			Compression string `json:"compression"`
			RPOHours    int    `json:"rpo_hours"`
			HasCommands bool   `json:"has_commands"`
		} `json:"options"`
		Enabled bool `json:"enabled"`
	}
	create := func(tok, body string) (int, jobOut, string) {
		t.Helper()
		code, out := apiPost(t, e, tok, "/api/v1/jobs", body)
		var j jobOut
		json.Unmarshal([]byte(out), &j)
		return code, j, out
	}

	// A file job with schedule, retention and options.
	src := t.TempDir()
	code, files, out := create(op, fmt.Sprintf(`{"name":"Documents","kind":"files","agent_id":%d,"target_id":%d,"paths":[%q],"excludes":["*.tmp"],
		"schedule":{"kind":"daily","days":[1,2,3,4,5],"times":["22:00"]},"retention":{"keep_daily":14,"keep_weekly":8},
		"options":{"compression":"fast","rpo_hours":26}}`, ag, t1, src))
	if code != 202 || files.ID == 0 || files.Kind != "files" || files.ScheduleSpec.Kind != "daily" || len(files.ScheduleSpec.Days) != 5 ||
		files.Retention.KeepDaily != 14 || files.Options.Compression != "fast" || files.Options.RPOHours != 26 || !files.Enabled {
		t.Fatalf("create files job: %d %s", code, out)
	}
	if j, _ := e.store.GetJob(ctx, files.ID); j.Retention.KeepWeekly != 8 || j.Retention.KeepMonthly != 0 || len(j.Excludes) != 1 {
		t.Errorf("stored job: %+v", j)
	}
	// Without retention a job gets the console's defaults.
	code, plain, out := create(op, fmt.Sprintf(`{"name":"Plain","kind":"files","agent_id":%d,"target_id":%d,"paths":[%q]}`, ag, t1, src))
	if code != 202 || plain.Retention.KeepDaily != 7 || plain.ScheduleSpec.Kind != "manual" {
		t.Errorf("defaults: %d %s", code, out)
	}

	// Commands need the administrator role.
	cmd := fmt.Sprintf(`{"name":"With commands","kind":"files","agent_id":%d,"target_id":%d,"paths":[%q],"options":{"pre_command":"echo hi"}}`, ag, t1, src)
	if code, _, _ := create(op, cmd); code != 403 {
		t.Errorf("operator set commands: %d", code)
	}
	if code, j, out := create(adm, cmd); code != 202 || !j.Options.HasCommands || strings.Contains(out, "echo hi") {
		t.Errorf("admin commands: %d %s", code, out)
	}
	if code, _, _ := create(help, fmt.Sprintf(`{"name":"x","kind":"files","agent_id":%d,"target_id":%d,"paths":["/x"]}`, ag, t1)); code != 403 {
		t.Errorf("restore operator created a job: %d", code)
	}

	// Image jobs: ticking every partition means the whole disk.
	code, img, out := create(op, fmt.Sprintf(`{"name":"PC system","kind":"image","agent_id":%d,"target_id":%d,"image_disk":0,"image_partitions":[3]}`, ag, t1))
	if code != 202 || img.ImageDisk == nil || *img.ImageDisk != 0 || len(img.ImagePartitions) != 1 || img.ImagePartitions[0] != 3 {
		t.Errorf("image job: %d %s", code, out)
	}
	code, whole, out := create(op, fmt.Sprintf(`{"name":"PC whole","kind":"image","agent_id":%d,"target_id":%d,"image_disk":0,"image_partitions":[1,3]}`, ag, t1))
	if code != 202 || len(whole.ImagePartitions) != 0 {
		t.Errorf("whole disk: %d %s", code, out)
	}
	if code, all, out := create(op, fmt.Sprintf(`{"name":"PC all","kind":"image","agent_id":%d,"target_id":%d,"image_disk":-1}`, ag, t1)); code != 202 || all.ImageDisk == nil || *all.ImageDisk != -1 {
		t.Errorf("all disks: %d %s", code, out)
	}

	// A copy job takes the agent of its source.
	code, cp, out := create(op, fmt.Sprintf(`{"name":"Offsite","kind":"copy","source_job_id":%d,"target_id":%d,"schedule":{"kind":"after"}}`, files.ID, t2))
	if code != 202 || cp.AgentID != ag || cp.ScheduleSpec.Kind != "after" {
		t.Errorf("copy job: %d %s", code, out)
	}

	// Mistakes are 400 with the console's message; nothing is stored.
	before, _ := e.store.ListJobs(ctx)
	for _, bad := range []string{
		``,
		fmt.Sprintf(`{"name":"x","kind":"vm","agent_id":%d,"target_id":%d}`, ag, t1),
		fmt.Sprintf(`{"name":"x","kind":"files","agent_id":%d,"target_id":%d}`, ag, t1),
		fmt.Sprintf(`{"name":"x","kind":"files","agent_id":999,"target_id":%d,"paths":["/x"]}`, t1),
		fmt.Sprintf(`{"name":"x","kind":"files","agent_id":%d,"target_id":999,"paths":["/x"]}`, ag),
		fmt.Sprintf(`{"name":"x","kind":"files","agent_id":%d,"target_id":%d,"paths":["/x"],"image_disk":0}`, ag, t1),
		fmt.Sprintf(`{"name":"x","kind":"image","agent_id":%d,"target_id":%d,"image_disk":5}`, ag, t1),
		fmt.Sprintf(`{"name":"x","kind":"image","agent_id":%d,"target_id":%d,"image_disk":-1,"image_partitions":[1]}`, ag, t1),
		fmt.Sprintf(`{"name":"x","kind":"files","agent_id":%d,"target_id":%d,"paths":["/x"],"schedule":{"kind":"daily","times":["25:00"]}}`, ag, t1),
		fmt.Sprintf(`{"name":"x","kind":"files","agent_id":%d,"target_id":%d,"paths":["/x"],"options":{"compression":"ultra"}}`, ag, t1),
		fmt.Sprintf(`{"name":"x","kind":"files","agent_id":%d,"target_id":%d,"paths":["/x"],"shedule":{}}`, ag, t1),
		fmt.Sprintf(`{"name":"x","kind":"copy","source_job_id":%d,"target_id":%d}`, files.ID, t1),
	} {
		if code, out := apiPost(t, e, op, "/api/v1/jobs", bad); code != 400 || !strings.Contains(out, `"error"`) {
			t.Errorf("bad job %s: %d %s", bad, code, out)
		}
	}
	if after, _ := e.store.ListJobs(ctx); len(after) != len(before) {
		t.Errorf("a bad request stored a job: %d -> %d", len(before), len(after))
	}

	// Changing a job keeps what is not given.
	p := fmt.Sprintf("/api/v1/jobs/%d", files.ID)
	code, out = apiSend(t, e, op, "PATCH", p, `{"name":"Documents (2h)","schedule":{"kind":"interval","every_minutes":120,"from_hour":7,"to_hour":19},"retention":{"keep_daily":30,"keep_weekly":8}}`)
	var ch jobOut
	json.Unmarshal([]byte(out), &ch)
	if code != 200 || ch.Name != "Documents (2h)" || ch.ScheduleSpec.Kind != "interval" || ch.ScheduleSpec.EveryMinutes != 120 ||
		ch.Retention.KeepDaily != 30 || len(ch.Excludes) != 1 || ch.Options.Compression != "fast" || len(ch.Paths) != 1 {
		t.Errorf("update: %d %s", code, out)
	}
	for _, bad := range []string{`{"kind":"image"}`, `{"agent_id":5}`, `{"schedule":{"kind":"monthly","day_of_month":40,"times":["22:00"]}}`, `{"name":"  "}`} {
		if code, out := apiSend(t, e, op, "PATCH", p, bad); code != 400 {
			t.Errorf("bad update %s: %d %s", bad, code, out)
		}
	}
	if code, _ := apiSend(t, e, op, "PATCH", "/api/v1/jobs/99999", `{"name":"x"}`); code != 404 {
		t.Errorf("update of a missing job: %d", code)
	}
	if j, _ := e.store.GetJob(ctx, files.ID); j.Name != "Documents (2h)" {
		t.Errorf("a bad update changed the job: %q", j.Name)
	}

	// Deleting.
	if code, _ := apiSend(t, e, help, "DELETE", fmt.Sprintf("/api/v1/jobs/%d", plain.ID), ""); code != 403 {
		t.Errorf("restore operator deleted a job: %d", code)
	}
	if code, out := apiSend(t, e, op, "DELETE", fmt.Sprintf("/api/v1/jobs/%d", plain.ID), ""); code != 200 || !strings.Contains(out, `"deleted":true`) {
		t.Errorf("delete: %d %s", code, out)
	}
	if code, _ := apiCall(t, e, op, "GET", fmt.Sprintf("/api/v1/jobs/%d", plain.ID)); code != 404 {
		t.Errorf("deleted job still there: %d", code)
	}

	// With four-eyes approval, shorter retention and deleting wait for a
	// second user in the console.
	pw := "correct-horse-9"
	admin.do("POST", "/users", url.Values{"username": {"admin2"}, "role": {"admin"}, "password": {pw}, "password2": {pw}})
	if _, loc, _ := admin.do("POST", "/settings/four-eyes", url.Values{"four_eyes": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("four-eyes: %s", loc)
	}
	if code, _ := apiSend(t, e, op, "PATCH", p, `{"retention":{"keep_daily":1}}`); code != 409 {
		t.Errorf("shorter retention with four-eyes: %d", code)
	}
	if code, _ := apiSend(t, e, op, "PATCH", p, `{"retention":{"keep_daily":60,"keep_weekly":8}}`); code != 200 {
		t.Errorf("longer retention with four-eyes: %d", code)
	}
	if code, _ := apiSend(t, e, op, "DELETE", p, ""); code != 409 {
		t.Errorf("delete with four-eyes: %d", code)
	}
}

// TestAPIFiles browses and downloads the files of a backup over the REST
// API.
func TestAPIFiles(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "s3", Kind: "s3", URL: "s3://" + s3.Host + "/backups/files?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	if _, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "test"); err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "docs", "deep"), 0o755)
	os.WriteFile(filepath.Join(src, "docs", "report.txt"), []byte("quarterly report"), 0o644)
	os.WriteFile(filepath.Join(src, "docs", "deep", "note.md"), []byte("a note"), 0o644)
	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: a.ID, TargetID: targetID, Name: "Docs", Paths: []string{src}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
	run, err := e.store.ClaimRun(ctx, a.ID)
	if err != nil || run == nil || run.ID != runID {
		t.Fatalf("claim: %+v %v", run, err)
	}
	be, err := backend.Open(ctx, run.RepoURL, backend.Options{S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password(""))
	if err != nil {
		t.Fatal(err)
	}
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := e.store.FinishRun(ctx, a.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: sn.ID.String()}); err != nil {
		t.Fatal(err)
	}
	comps, _ := fsutil.ParseSnapshotPath(src)
	docs := strings.Join(append(append([]string{}, comps...), "docs"), "/")

	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	help := apiToken(t, admin, "restore")
	view := apiToken(t, admin, "viewer")

	// Browse.
	if code, _ := apiCall(t, e, view, "GET", fmt.Sprintf("/api/v1/runs/%d/files", runID)); code != 403 {
		t.Errorf("viewer browsed files: %d", code)
	}
	code, out := apiCall(t, e, help, "GET", fmt.Sprintf("/api/v1/runs/%d/files?path=%s", runID, url.QueryEscape(docs)))
	var ls struct {
		Path    string `json:"path"`
		Entries []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Dir  bool   `json:"dir"`
			Size uint64 `json:"size"`
		} `json:"entries"`
	}
	json.Unmarshal([]byte(out), &ls)
	found := map[string]bool{}
	for _, x := range ls.Entries {
		found[fmt.Sprintf("%s/%v", x.Name, x.Dir)] = true
		if x.Name == "report.txt" && x.Size != 16 {
			t.Errorf("size of report.txt: %d", x.Size)
		}
		if x.Name == "report.txt" && x.Path != docs+"/report.txt" {
			t.Errorf("entry path: %s", x.Path)
		}
	}
	if code != 200 || ls.Path != docs || !found["report.txt/false"] || !found["deep/true"] {
		t.Errorf("list: %d %s", code, out)
	}
	if code, _ := apiCall(t, e, help, "GET", fmt.Sprintf("/api/v1/runs/%d/files?path=%s", runID, url.QueryEscape(docs+"/nope"))); code != 404 {
		t.Errorf("missing folder: %d", code)
	}

	// Download one file as is, a folder as a ZIP.
	download := func(tok, body string) (*http.Response, []byte) {
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/runs/%d/files/download", e.ts.URL, runID), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	resp, body := download(help, fmt.Sprintf(`{"paths":[%q]}`, docs+"/report.txt"))
	if resp.StatusCode != 200 || string(body) != "quarterly report" || !strings.Contains(resp.Header.Get("Content-Disposition"), "report.txt") {
		t.Errorf("file: %d %q %s", resp.StatusCode, body, resp.Header.Get("Content-Disposition"))
	}
	resp, body = download(help, fmt.Sprintf(`{"paths":[%q]}`, docs))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("folder: %d %s", resp.StatusCode, body)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["docs/report.txt"] || !names["docs/deep/note.md"] {
		t.Errorf("zip: %v", names)
	}
	if resp, body := download(help, fmt.Sprintf(`{"paths":[%q]}`, docs+"/nope.txt")); resp.StatusCode != 404 || !strings.Contains(string(body), "not in the backup") {
		t.Errorf("missing file: %d %s", resp.StatusCode, body)
	}
	if resp, _ := download(help, `{"paths":[]}`); resp.StatusCode != 400 {
		t.Errorf("no paths: %d", resp.StatusCode)
	}
	if resp, _ := download(view, fmt.Sprintf(`{"paths":[%q]}`, docs)); resp.StatusCode != 403 {
		t.Errorf("viewer downloaded: %d", resp.StatusCode)
	}
}
