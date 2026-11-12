package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const customer = "7f3a9e2b-1c4d-4e5f-8a9b-0c1d2e3f4a5b"

// fakes holds the three services the connector talks to.
type fakes struct {
	t  *testing.T
	mu sync.Mutex

	// platform
	queue     []Command
	completed map[string]map[string]any
	statuses  []map[string]any
	uploads   map[string][]byte

	// Backblaze
	b2Keys     []map[string]any
	b2Deleted  []string
	bucketType string
	lock       bool

	// BackupZit
	targets       []map[string]any
	targetPosts   []map[string]any
	failTarget    bool
	testsUntilOK  int
	jobPosts      []map[string]any
	jobPatches    []map[string]any
	lastAuthToken string
}

func newFakes(t *testing.T) (*fakes, *connector) {
	f := &fakes{t: t, completed: map[string]map[string]any{}, uploads: map[string][]byte{}, bucketType: "allPrivate", lock: true}
	bz := httptest.NewServer(http.HandlerFunc(f.backupZit))
	pf := httptest.NewServer(http.HandlerFunc(f.platform))
	bb := httptest.NewServer(http.HandlerFunc(f.backblaze))
	t.Cleanup(func() { bz.Close(); pf.Close(); bb.Close() })
	cfg := &Config{RunDays: 90, TempDir: t.TempDir()}
	cfg.Platform.URL, cfg.Platform.Token = pf.URL, "pf-token"
	cfg.BackupZit.URL, cfg.BackupZit.Token = bz.URL, "bz-token"
	cfg.B2.KeyID, cfg.B2.AppKey, cfg.B2.Bucket, cfg.B2.LockDays = "prov-id", "prov-secret", "itdocs-bz-backups", 30
	c := newConnector(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.b2.authURL = bb.URL
	c.pf.http = pf.Client()
	retryDelay = 10 * time.Millisecond
	return f, c
}

func readBody(r *http.Request) map[string]any {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakes) platform(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/upload/") {
		b, _ := io.ReadAll(r.Body)
		f.uploads[strings.TrimPrefix(r.URL.Path, "/upload/")] = b
		w.WriteHeader(200)
		return
	}
	if r.Header.Get("Authorization") != "Bearer pf-token" {
		writeJSON(w, 401, map[string]string{"error": "bad token"})
		return
	}
	switch {
	case r.URL.Path == "/commands/claim":
		q := f.queue
		f.queue = nil
		writeJSON(w, 200, map[string]any{"commands": q})
	case strings.HasPrefix(r.URL.Path, "/commands/") && strings.HasSuffix(r.URL.Path, "/complete"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/commands/"), "/complete")
		f.completed[id] = readBody(r)
		writeJSON(w, 200, map[string]any{})
	case r.URL.Path == "/status":
		f.statuses = append(f.statuses, readBody(r))
		writeJSON(w, 200, map[string]any{})
	case r.URL.Path == "/downloads/upload-url":
		b := readBody(r)
		p := fmt.Sprintf("%s/%s", b["command_id"], b["filename"])
		writeJSON(w, 200, map[string]any{"storage_path": p, "signed_url": "http://" + r.Host + "/upload/" + p})
	default:
		writeJSON(w, 404, map[string]string{"error": "no route"})
	}
}

func (f *fakes) backblaze(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/b2api/v2/b2_authorize_account":
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("prov-id:prov-secret")) {
			writeJSON(w, 401, map[string]string{"message": "bad key"})
			return
		}
		writeJSON(w, 200, map[string]any{"accountId": "acct1", "apiUrl": "http://" + r.Host, "authorizationToken": "tok",
			"s3ApiUrl": "https://s3.eu-central-003.backblazeb2.com"})
	case "/b2api/v2/b2_list_buckets":
		b := readBody(r)
		if b["accountId"] != "acct1" || r.Header.Get("Authorization") != "tok" {
			writeJSON(w, 401, map[string]string{"message": "unauthorized"})
			return
		}
		writeJSON(w, 200, map[string]any{"buckets": []any{map[string]any{"bucketId": "bkt1", "bucketName": b["bucketName"],
			"bucketType": f.bucketType, "fileLockConfiguration": map[string]any{"isClientAuthorizedToRead": true,
				"value": map[string]any{"isFileLockEnabled": f.lock}}}}})
	case "/b2api/v2/b2_create_key":
		b := readBody(r)
		f.b2Keys = append(f.b2Keys, b)
		writeJSON(w, 200, map[string]any{"applicationKeyId": fmt.Sprintf("K%d", len(f.b2Keys)), "applicationKey": "secret"})
	case "/b2api/v2/b2_delete_key":
		f.b2Deleted = append(f.b2Deleted, fmt.Sprint(readBody(r)["applicationKeyId"]))
		writeJSON(w, 200, map[string]any{})
	default:
		writeJSON(w, 404, map[string]string{"message": "no route"})
	}
}

func (f *fakes) backupZit(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAuthToken = r.Header.Get("Authorization")
	if f.lastAuthToken != "Bearer bz-token" {
		writeJSON(w, 401, map[string]string{"error": "missing, invalid or expired API token"})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case r.Method == "GET" && p == "/status":
		writeJSON(w, 200, map[string]any{"version": "0.48.2+itdocs.1", "agents": 1})
	case r.Method == "GET" && p == "/targets":
		writeJSON(w, 200, f.targets)
	case r.Method == "POST" && p == "/targets":
		b := readBody(r)
		f.targetPosts = append(f.targetPosts, b)
		if f.failTarget {
			writeJSON(w, 400, map[string]string{"error": "S3 access key is required"})
			return
		}
		t := map[string]any{"id": len(f.targets) + 1, "name": b["name"], "kind": "s3",
			"location": fmt.Sprintf("s3://%s/%s/%s", b["s3_endpoint"], b["s3_bucket"], b["s3_prefix"]), "encrypted": true, "immutable_days": b["s3_lock_days"]}
		f.targets = append(f.targets, t)
		writeJSON(w, 202, map[string]any{"target": t, "recovery_key": "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF-GGGG-HHHH",
			"test": map[string]any{"ok": f.testsUntilOK == 0, "message": "test"}})
	case r.Method == "POST" && strings.HasPrefix(p, "/targets/") && strings.HasSuffix(p, "/test"):
		if f.testsUntilOK > 0 {
			f.testsUntilOK--
		}
		writeJSON(w, 202, map[string]any{"ok": f.testsUntilOK == 0, "message": "retest"})
	case r.Method == "POST" && p == "/jobs":
		b := readBody(r)
		f.jobPosts = append(f.jobPosts, b)
		writeJSON(w, 202, map[string]any{"id": 9, "name": b["name"]})
	case r.Method == "PATCH" && p == "/jobs/9":
		f.jobPatches = append(f.jobPatches, readBody(r))
		writeJSON(w, 200, map[string]any{"id": 9})
	case r.Method == "POST" && p == "/jobs/9/run":
		writeJSON(w, 202, map[string]any{"run_id": 77})
	case r.Method == "POST" && p == "/runs/77/files/download":
		w.Header().Set("Content-Disposition", `attachment; filename="Rechnung 4711.pdf"`)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("PDF-INHALT"))
	case r.Method == "POST" && p == "/runs/78/files/download":
		writeJSON(w, 404, map[string]string{"error": "x is not in the backup"})
	case r.Method == "GET" && p == "/agents":
		writeJSON(w, 200, []any{map[string]any{"id": 12, "hostname": "PC-01"}})
	case r.Method == "GET" && p == "/agents/12":
		writeJSON(w, 200, map[string]any{"id": 12, "hostname": "PC-01", "disks": []any{map[string]any{"number": 0}}})
	case r.Method == "GET" && p == "/storage":
		writeJSON(w, 200, []any{map[string]any{"target_id": 1, "used_bytes": 1234}})
	case r.Method == "GET" && p == "/jobs":
		writeJSON(w, 200, []any{map[string]any{"id": 9}})
	case r.Method == "GET" && p == "/runs":
		old := time.Now().AddDate(0, 0, -200).Format(time.RFC3339)
		writeJSON(w, 200, []any{map[string]any{"id": 77, "status": "success", "queued_at": time.Now().Format(time.RFC3339)},
			map[string]any{"id": 5, "status": "success", "queued_at": old}})
	default:
		writeJSON(w, 404, map[string]string{"error": "no route " + r.Method + " " + p})
	}
}

func (f *fakes) enqueue(action string, params any) string {
	b, _ := json.Marshal(params)
	id := fmt.Sprintf("cmd-%d", len(f.completed)+len(f.queue)+1)
	f.mu.Lock()
	f.queue = append(f.queue, Command{ID: id, CustomerID: customer, Action: action, Params: b})
	f.mu.Unlock()
	return id
}

func (f *fakes) result(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completed[id]
}

func process(t *testing.T, c *connector) {
	t.Helper()
	if err := c.processCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProvision(t *testing.T) {
	f, c := newFakes(t)
	f.testsUntilOK = 2 // the new key needs a moment
	id := f.enqueue("target.provision", map[string]string{"name": "Müller GmbH"})
	process(t, c)
	res := f.result(id)
	if res["status"] != "done" {
		t.Fatalf("provision: %v", res)
	}
	r := res["result"].(map[string]any)
	if r["recovery_key"] == "" || r["prefix"] != "c-"+customer || r["test"].(map[string]any)["ok"] != true {
		t.Errorf("result: %v", r)
	}
	if len(f.b2Keys) != 1 {
		t.Fatalf("keys: %v", f.b2Keys)
	}
	k := f.b2Keys[0]
	caps := fmt.Sprint(k["capabilities"])
	if k["bucketId"] != "bkt1" || k["namePrefix"] != "c-"+customer+"/" || strings.Contains(caps, "writeKeys") ||
		strings.Contains(caps, "bypassGovernance") || strings.Contains(caps, "writeBuckets") || strings.Contains(caps, "deleteBuckets") ||
		!strings.Contains(caps, "writeFileRetentions") {
		t.Errorf("customer key: %v", k)
	}
	tp := f.targetPosts[0]
	if tp["s3_endpoint"] != "s3.eu-central-003.backblazeb2.com" || tp["s3_region"] != "eu-central-003" || tp["s3_bucket"] != "itdocs-bz-backups" ||
		tp["s3_prefix"] != "c-"+customer || tp["s3_access_key"] != "K1" || tp["s3_secret_key"] != "secret" || tp["s3_lock_days"] != float64(30) ||
		tp["name"] != "Müller GmbH" {
		t.Errorf("target body: %v", tp)
	}
	// The key never goes back to the platform.
	if b, _ := json.Marshal(res); strings.Contains(string(b), `"secret"`) {
		t.Errorf("customer key in the result: %s", b)
	}

	// A second provision for the same customer is refused.
	id = f.enqueue("target.provision", map[string]string{"name": "Müller GmbH"})
	process(t, c)
	if res := f.result(id); res["status"] != "failed" || !strings.Contains(fmt.Sprint(res["error"]), "bereits") || len(f.b2Keys) != 1 {
		t.Errorf("second provision: %v", res)
	}
}

func TestProvisionFailures(t *testing.T) {
	f, c := newFakes(t)
	// BackupZit refuses the target: the new key is deleted again.
	f.failTarget = true
	id := f.enqueue("target.provision", map[string]string{"name": "x"})
	process(t, c)
	if res := f.result(id); res["status"] != "failed" || !strings.Contains(fmt.Sprint(res["error"]), "abgelehnt") {
		t.Errorf("refused target: %v", res)
	}
	if len(f.b2Deleted) != 1 || f.b2Deleted[0] != "K1" {
		t.Errorf("orphaned key not deleted: %v", f.b2Deleted)
	}
	f.failTarget = false

	// No Object Lock, public bucket, bad customer id, no B2 setup.
	f.lock = false
	id = f.enqueue("target.provision", map[string]string{})
	process(t, c)
	if res := f.result(id); !strings.Contains(fmt.Sprint(res["error"]), "Object Lock") {
		t.Errorf("no object lock: %v", res)
	}
	f.lock, f.bucketType = true, "allPublic"
	id = f.enqueue("target.provision", map[string]string{})
	process(t, c)
	if res := f.result(id); !strings.Contains(fmt.Sprint(res["error"]), "private") {
		t.Errorf("public bucket: %v", res)
	}
	f.bucketType = "allPrivate"
	f.mu.Lock()
	f.queue = append(f.queue, Command{ID: "bad", CustomerID: "../other", Action: "target.provision", Params: json.RawMessage(`{}`)})
	f.mu.Unlock()
	process(t, c)
	if res := f.result("bad"); !strings.Contains(fmt.Sprint(res["error"]), "Kunden-ID") {
		t.Errorf("bad customer id: %v", res)
	}
	c.cfg.B2.AppKey = ""
	id = f.enqueue("target.provision", map[string]string{})
	process(t, c)
	if res := f.result(id); !strings.Contains(fmt.Sprint(res["error"]), "nicht eingerichtet") {
		t.Errorf("no b2: %v", res)
	}
	if len(f.b2Keys) != 1 {
		t.Errorf("keys created on failures: %v", f.b2Keys)
	}
}

func TestJobs(t *testing.T) {
	f, c := newFakes(t)
	id := f.enqueue("job.create", map[string]any{
		"name": "PC-01", "kind": "image", "agent_id": 12, "target_id": 3, "image_disk": 0, "image_partitions": []int{3},
		"encryption_password": "geheim", "device_id": "dev-1",
		"schedule":  map[string]any{"kind": "daily", "days": []int{1, 2, 3, 4, 5}, "times": []string{"22:00"}, "ext": map[string]any{"months": []int{1}, "on_boot": false}},
		"retention": map[string]any{"keep_daily": 14, "ext": map[string]any{"mode": "count"}},
		"options":   map[string]any{"compression": "fast", "ext": map[string]any{"physical_seeding": false}, "unknown": 1},
	})
	process(t, c)
	res := f.result(id)
	if res["status"] != "done" || res["result"].(map[string]any)["id"] != float64(9) {
		t.Fatalf("create: %v", res)
	}
	body := f.jobPosts[0]
	if _, ok := body["encryption_password"]; ok {
		t.Error("password passed on")
	}
	if _, ok := body["device_id"]; ok {
		t.Error("device_id passed on")
	}
	if s := body["schedule"].(map[string]any); s["ext"] != nil || s["kind"] != "daily" {
		t.Errorf("schedule: %v", s)
	}
	if o := body["options"].(map[string]any); o["ext"] != nil || o["unknown"] != nil || o["compression"] != "fast" {
		t.Errorf("options: %v", o)
	}
	w := fmt.Sprint(res["result"].(map[string]any)["warnings"])
	if !strings.Contains(w, "Kennwort") || !strings.Contains(w, "Zeitplan") || !strings.Contains(w, "Alter") {
		t.Errorf("warnings: %s", w)
	}
	if strings.Contains(w, "Leistungsfenster") {
		t.Errorf("empty ext warned: %s", w)
	}

	// Update: only changeable fields go through.
	id = f.enqueue("job.update", map[string]any{"job_id": 9, "kind": "files", "agent_id": 1, "name": "neu", "paths": []string{"C:\\Daten"}})
	process(t, c)
	if res := f.result(id); res["status"] != "done" {
		t.Fatalf("update: %v", res)
	}
	if p := f.jobPatches[0]; p["kind"] != nil || p["agent_id"] != nil || p["job_id"] != nil || p["name"] != "neu" {
		t.Errorf("patch: %v", p)
	}

	// Local and network destinations are not made yet.
	id = f.enqueue("job.create", map[string]any{"name": "x", "kind": "files", "destination": map[string]any{"type": "network", "path": `\\nas\b`}})
	process(t, c)
	if res := f.result(id); res["status"] != "failed" || !strings.Contains(fmt.Sprint(res["error"]), "Netzwerk") || len(f.jobPosts) != 1 {
		t.Errorf("network destination: %v", res)
	}
	id = f.enqueue("job.run", map[string]any{"job_id": 9})
	process(t, c)
	if res := f.result(id); res["result"].(map[string]any)["run_id"] != float64(77) {
		t.Errorf("run: %v", res)
	}
	id = f.enqueue("job.run", map[string]any{})
	process(t, c)
	if res := f.result(id); !strings.Contains(fmt.Sprint(res["error"]), "job_id fehlt") {
		t.Errorf("run without id: %v", res)
	}
}

func TestDownloadAndOthers(t *testing.T) {
	f, c := newFakes(t)
	id := f.enqueue("files.download", map[string]any{"run_id": 77, "paths": []string{"C/a.pdf"}, "password": "x"})
	process(t, c)
	res := f.result(id)
	r, _ := res["result"].(map[string]any)
	if res["status"] != "done" || r["storage_path"] != id+"/Rechnung 4711.pdf" || r["size"] != float64(10) {
		t.Fatalf("download: %v", res)
	}
	if string(f.uploads[id+"/Rechnung 4711.pdf"]) != "PDF-INHALT" {
		t.Errorf("uploaded: %q", f.uploads)
	}
	if left, _ := filepath.Glob(filepath.Join(c.cfg.TempDir, "bzc-download-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	id = f.enqueue("files.download", map[string]any{"run_id": 78, "paths": []string{"x"}})
	process(t, c)
	if res := f.result(id); res["status"] != "failed" || !strings.Contains(fmt.Sprint(res["error"]), "not in the backup") {
		t.Errorf("missing file: %v", res)
	}
	for action, want := range map[string]string{"restore.machine": "noch nicht umgesetzt", "delete.everything": "unbekannte Auftragsart"} {
		id = f.enqueue(action, map[string]any{})
		process(t, c)
		if res := f.result(id); res["status"] != "failed" || !strings.Contains(fmt.Sprint(res["error"]), want) {
			t.Errorf("%s: %v", action, res)
		}
	}
	id = f.enqueue("restore.to_folder", map[string]any{"run_id": 77})
	process(t, c)
	if res := f.result(id); !strings.Contains(fmt.Sprint(res["error"]), "Zielordner") {
		t.Errorf("restore without folder: %v", res)
	}
	// A wrong console token is reported in words.
	c.bz.token = "wrong"
	id = f.enqueue("job.run", map[string]any{"job_id": 9})
	process(t, c)
	if res := f.result(id); !strings.Contains(fmt.Sprint(res["error"]), "svc-connector") {
		t.Errorf("bad console token: %v", res)
	}
}

func TestStatus(t *testing.T) {
	f, c := newFakes(t)
	c.syncStatus(context.Background())
	if len(f.statuses) != 1 {
		t.Fatalf("statuses: %d", len(f.statuses))
	}
	st := f.statuses[0]
	agents := st["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["disks"] == nil {
		t.Errorf("agents: %v", agents)
	}
	if runs := st["runs"].([]any); len(runs) != 1 {
		t.Errorf("old runs not filtered: %v", runs)
	}
	if tg := st["targets"]; tg == nil {
		t.Errorf("targets: %v", st)
	}
	// Unchanged runs are not sent again.
	c.syncRecentRuns(context.Background())
	if len(f.statuses) != 1 {
		t.Errorf("unchanged runs sent: %d", len(f.statuses))
	}
	c.mu.Lock()
	c.runsSent[77] = "running|<nil>"
	c.mu.Unlock()
	c.syncRecentRuns(context.Background())
	if len(f.statuses) != 2 || len(f.statuses[1]["runs"].([]any)) != 1 {
		t.Errorf("changed run not sent: %v", f.statuses)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	os.WriteFile(p, []byte(`{"platform":{"url":"https://x.supabase.co/functions/v1/backupzit-connector","token":"t"},
		"backupzit":{"token":"b","fingerprint":"SHA256:abc"}}`), 0o644)
	if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("readable config accepted: %v", err)
	}
	os.Chmod(p, 0o600)
	c, err := loadConfig(p)
	if err != nil || c.BackupZit.URL != "https://127.0.0.1:8443" || c.B2.LockDays != 30 || c.PollSeconds != 5 || c.b2Ready() {
		t.Errorf("defaults: %+v %v", c, err)
	}
	ex, _ := os.ReadFile("packaging/config.example.json")
	os.WriteFile(p, ex, 0o600)
	if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), "not filled in") {
		t.Errorf("example config accepted: %v", err)
	}
	os.WriteFile(p, []byte(`{"platform":{"url":"http://x","token":"t"},"backupzit":{"token":"b","fingerprint":"x"}}`), 0o600)
	if _, err := loadConfig(p); err == nil {
		t.Error("http platform accepted")
	}
}
