package server_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
)

// TestAPIManage sets BackupZit up over the REST API: storage targets,
// enrollment codes and the disks of an agent.
func TestAPIManage(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	adm := apiToken(t, admin, "admin")
	op := apiToken(t, admin, "operator")
	view := apiToken(t, admin, "viewer")

	// A local target: encrypted by default, with its recovery key once.
	dir := filepath.Join(t.TempDir(), "nas")
	body := fmt.Sprintf(`{"name":"Customer A","kind":"local","url":%q}`, dir)
	if code, _ := apiPost(t, e, op, "/api/v1/targets", body); code != 403 {
		t.Errorf("operator created a target: %d", code)
	}
	code, out := apiPost(t, e, adm, "/api/v1/targets", body)
	var res struct {
		Target struct {
			ID        int64 `json:"id"`
			Encrypted bool  `json:"encrypted"`
		} `json:"target"`
		RecoveryKey string `json:"recovery_key"`
		Test        *struct {
			OK      bool   `json:"ok"`
			Message string `json:"message"`
		} `json:"test"`
	}
	json.Unmarshal([]byte(out), &res)
	if code != 202 || res.Target.ID == 0 || !res.Target.Encrypted || len(res.RecoveryKey) != 39 || res.Test != nil {
		t.Fatalf("create local target: %d %s", code, out)
	}
	tg, err := e.store.GetTarget(ctx, res.Target.ID)
	if err != nil || tg.Name != "Customer A" || tg.RecoveryKey != res.RecoveryKey {
		t.Errorf("stored target: %+v %v", tg, err)
	}

	// S3 with Object Lock and an own recovery key; the storage cannot be
	// reached, so the target is created and the test reports the failure.
	key := "K7QM-2XRD-AAAA-BBBB-CCCC-DDDD-EEEE-FFFF"
	code, out = apiPost(t, e, adm, "/api/v1/targets", `{"name":"Customer B","kind":"s3","s3_endpoint":"127.0.0.1:1","s3_bucket":"b",
		"s3_prefix":"pc","s3_http":true,"s3_region":"eu-central-003","s3_access_key":"id","s3_secret_key":"secret","s3_lock_days":30,
		"recovery_key":"`+key+`"}`)
	res.Test = nil
	json.Unmarshal([]byte(out), &res)
	if code != 202 || res.Test == nil || res.Test.OK || !strings.Contains(res.Test.Message, "connection test failed") || res.RecoveryKey != key {
		t.Fatalf("create s3 target: %d %s", code, out)
	}
	if tg, _ := e.store.GetTarget(ctx, res.Target.ID); tg.S3LockDays != 30 || tg.URL != "s3://127.0.0.1:1/b/pc?tls=false" || tg.S3SecretKey != "secret" {
		t.Errorf("stored s3 target: %+v", tg)
	}
	code, out = apiPost(t, e, adm, fmt.Sprintf("/api/v1/targets/%d/test", res.Target.ID), "")
	if code != 202 || !strings.Contains(out, `"ok":false`) {
		t.Errorf("test target: %d %s", code, out)
	}
	if code, _ := apiPost(t, e, op, fmt.Sprintf("/api/v1/targets/%d/test", res.Target.ID), ""); code != 403 {
		t.Errorf("operator tested a target: %d", code)
	}
	// Credentials never come back in the list.
	if _, out := apiCall(t, e, view, "GET", "/api/v1/targets"); strings.Contains(out, "secret") || strings.Contains(out, key) || !strings.Contains(out, `"immutable_days":30`) {
		t.Errorf("target list: %s", out)
	}

	// Unencrypted: no recovery key.
	code, out = apiPost(t, e, adm, "/api/v1/targets", fmt.Sprintf(`{"name":"plain","kind":"local","url":%q,"encrypted":false}`, filepath.Join(t.TempDir(), "p")))
	if code != 202 || strings.Contains(out, "recovery_key") || !strings.Contains(out, `"encrypted":false`) {
		t.Errorf("unencrypted target: %d %s", code, out)
	}

	// Mistakes are 400 with the console's message; nothing is stored.
	n, _ := e.store.ListTargets(ctx)
	for _, bad := range []string{
		``,
		`{"name":"x","kind":"s3","s3_endpoint":"h","s3_bucket":"b","s3_access_key":"a","s3_secret_key":"s","s3_lock_days":5000}`,
		`{"name":"x","kind":"s3","s3_endpoint":"h","s3_bucket":"b"}`,
		`{"name":"x","kind":"tape","url":"/x"}`,
		`{"name":"x","kind":"local","url":"/x","s3_bukcet":"typo"}`,
		`{"name":"x","kind":"local","url":"/x","recovery_key":"short"}`,
	} {
		if code, out := apiPost(t, e, adm, "/api/v1/targets", bad); code != 400 || !strings.Contains(out, `"error"`) {
			t.Errorf("bad target %s: %d %s", bad, code, out)
		}
	}
	if m, _ := e.store.ListTargets(ctx); len(m) != len(n) {
		t.Errorf("a bad request stored a target: %d -> %d", len(n), len(m))
	}

	// An enrollment code enrolls an agent.
	if code, _ := apiPost(t, e, view, "/api/v1/agents/enroll-code", ""); code != 403 {
		t.Errorf("viewer created an enrollment code: %d", code)
	}
	code, out = apiPost(t, e, op, "/api/v1/agents/enroll-code", "")
	var ec struct {
		Code        string    `json:"code"`
		Expires     time.Time `json:"expires"`
		Fingerprint string    `json:"fingerprint"`
	}
	json.Unmarshal([]byte(out), &ec)
	if code != 202 || !strings.HasPrefix(ec.Code, "BZ1-") || time.Until(ec.Expires) < 6*24*time.Hour || ec.Fingerprint != e.fp {
		t.Fatalf("enrollment code: %d %s", code, out)
	}
	_, tok, fp, err := api.DecodeEnrollCode(ec.Code)
	if err != nil || fp != e.fp {
		t.Fatalf("decode code: %v", err)
	}
	cfg, err := agent.Enroll(ctx, e.ts.URL, tok, fp, "test")
	if err != nil {
		t.Fatalf("enroll with the code: %v", err)
	}

	// A single-use code with a label: one agent only, which keeps the label.
	code, out = apiPost(t, e, op, "/api/v1/agents/enroll-code", `{"ttl_minutes":120,"single_use":true,"label":"itdocs:cust-1:dev-7"}`)
	json.Unmarshal([]byte(out), &ec)
	if code != 202 || time.Until(ec.Expires) > 121*time.Minute || time.Until(ec.Expires) < 119*time.Minute || !strings.Contains(out, `"single_use":true`) {
		t.Fatalf("single-use code: %d %s", code, out)
	}
	_, tok1, _, _ := api.DecodeEnrollCode(ec.Code)
	cfg1, err := agent.Enroll(ctx, e.ts.URL, tok1, e.fp, "test")
	if err != nil {
		t.Fatalf("enroll with the single-use code: %v", err)
	}
	if _, err := agent.Enroll(ctx, e.ts.URL, tok1, e.fp, "test"); err == nil {
		t.Error("a single-use code enrolled a second agent")
	}
	// The 7-day code stays usable for several agents.
	if _, err := agent.Enroll(ctx, e.ts.URL, tok, e.fp, "test"); err != nil {
		t.Errorf("second agent with the reusable code: %v", err)
	}
	for _, bad := range []string{`{"ttl_minutes":1}`, `{"ttl_minutes":50000}`, `{"label":"a\nb"}`, `{"label":"` + strings.Repeat("x", 201) + `"}`, `{"single":true}`} {
		if code, out := apiPost(t, e, op, "/api/v1/agents/enroll-code", bad); code != 400 {
			t.Errorf("bad code request %s: %d %s", bad, code, out)
		}
	}
	all, _ := e.store.ListAgents(ctx)
	var labelled int64
	for _, a := range all {
		if a.UUID == cfg1.AgentUUID {
			labelled = a.ID
		}
	}
	if _, out := apiCall(t, e, view, "GET", fmt.Sprintf("/api/v1/agents/%d", labelled)); !strings.Contains(out, `"enroll_label":"itdocs:cust-1:dev-7"`) {
		t.Errorf("label of the agent: %s", out)
	}
	if _, out := apiCall(t, e, view, "GET", "/api/v1/agents"); !strings.Contains(out, `"enroll_label":"itdocs:cust-1:dev-7"`) {
		t.Errorf("label in the agent list: %s", out)
	}

	// The disks an agent reported, for image jobs; not in the list.
	agents, _ := e.store.ListAgents(ctx)
	var id int64
	for _, a := range agents {
		if a.UUID == cfg.AgentUUID {
			id = a.ID
		}
	}
	inv := `[{"number":0,"model":"Msft Virtual Disk","size":68719476736,"sector_size":512,"style":"gpt","system":true,
	  "partitions":[{"number":1,"offset":1048576,"length":209715200,"gpt_type":"C12A7328-F81F-11D2-BA4B-00A0C93EC93B"},
	                {"number":3,"offset":227540992,"length":67554508800,"mount_points":["C:\\"],"file_system":"NTFS"}]}]`
	if err := e.store.TouchAgent(ctx, id, api.PollRequest{Disks: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}
	_, out = apiCall(t, e, view, "GET", fmt.Sprintf("/api/v1/agents/%d", id))
	if !strings.Contains(out, `"disks":[{"number":0,"model":"Msft Virtual Disk"`) || !strings.Contains(out, `{"number":3,"size":67554508800,`) ||
		!strings.Contains(out, `"mount_points":["C:\\"]`) || !strings.Contains(out, `{"number":1,"size":209715200,"kind":"EFI System"}`) {
		t.Errorf("agent disks: %s", out)
	}
	if _, out := apiCall(t, e, view, "GET", "/api/v1/agents"); strings.Contains(out, `"disks"`) {
		t.Errorf("agent list carries disks: %s", out)
	}
}
