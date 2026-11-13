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
	"github.com/max-zit/backupzit/internal/server"
)

// TestAPIImage covers disk image backups over the REST API: the disks of a
// backup, the space in use per partition, files from the image and a
// restore onto a disk.
func TestAPIImage(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	adm := apiToken(t, admin, "admin")
	view := apiToken(t, admin, "viewer")

	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "pc1")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	var a server.Agent
	for _, x := range agents {
		if x.UUID == cfg.AgentUUID {
			a = x
		}
	}
	inv := `[{"number":0,"size":68719476736,"sector_size":512,"style":"gpt","system":true,"gpt_disk_id":"AAAA-1",
	  "partitions":[{"number":1,"offset":1048576,"length":209715200},
	                {"number":3,"offset":227540992,"length":67554508800,"mount_points":["C:\\"],"file_system":"NTFS"}]},
	  {"number":1,"size":68719476736,"sector_size":512,"style":"mbr","partitions":[]}]`
	if err := e.store.TouchAgent(ctx, a.ID, api.PollRequest{Disks: json.RawMessage(inv)}, ""); err != nil {
		t.Fatal(err)
	}
	disk0 := 0
	jobID, err := e.store.CreateJob(ctx, server.Job{Kind: server.JobImage, AgentID: a.ID, TargetID: targetID, Name: "Image", ImageDisk: &disk0})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := e.store.QueueBackup(ctx, jobID, "manual")
	if c, err := e.store.ClaimRun(ctx, a.ID); err != nil || c == nil {
		t.Fatalf("claim: %v", err)
	}
	details := `{"number":0,"size":68719476736,"sector_size":512,"style":"gpt","gpt_disk_id":"AAAA-1","head":"` + strings.Repeat("ab", 32) + `",
	  "partitions":[{"number":1,"offset":1048576,"length":209715200,"included":false},
	                {"number":3,"offset":227540992,"length":67554508800,"mount_points":["C:\\"],"file_system":"NTFS","included":true,"method":"used-blocks","stored_bytes":25000000000}]}`
	if err := e.store.FinishRun(ctx, a.ID, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("cd", 32),
		Details: json.RawMessage(details)}); err != nil {
		t.Fatal(err)
	}

	// The run lists its disks; the NTFS volume is the folder "C".
	code, out := apiSend(t, e, view, "GET", fmt.Sprintf("/api/v1/runs/%d", runID), "")
	var run struct {
		ImageDisks []struct {
			Index      int `json:"index"`
			Number     int `json:"number"`
			Partitions []struct {
				Number   int    `json:"number"`
				Included bool   `json:"included"`
				Used     uint64 `json:"used"`
				Folder   string `json:"folder"`
			} `json:"partitions"`
		} `json:"image_disks"`
	}
	json.Unmarshal([]byte(out), &run)
	if code != 200 || len(run.ImageDisks) != 1 || len(run.ImageDisks[0].Partitions) != 2 ||
		run.ImageDisks[0].Partitions[1].Folder != "C" || run.ImageDisks[0].Partitions[1].Used != 25000000000 || run.ImageDisks[0].Partitions[0].Folder != "" {
		t.Fatalf("run: %d %s", code, out)
	}

	// The agent's partitions carry the space in use from that backup.
	code, out = apiSend(t, e, view, "GET", fmt.Sprintf("/api/v1/agents/%d", a.ID), "")
	if code != 200 || !strings.Contains(out, `"used":25000000000`) || strings.Count(out, `"used":`) != 1 {
		t.Errorf("agent: %d %s", code, out)
	}

	// Files: the volumes first; reading needs network storage.
	code, out = apiSend(t, e, adm, "GET", fmt.Sprintf("/api/v1/runs/%d/files", runID), "")
	if code != 200 || !strings.Contains(out, `"entries":[{"name":"C","path":"C","dir":true`) {
		t.Errorf("volumes: %d %s", code, out)
	}
	if code, out = apiSend(t, e, adm, "GET", fmt.Sprintf("/api/v1/runs/%d/files?path=C/Users", runID), ""); code != 409 {
		t.Errorf("files on a local disk: %d %s", code, out)
	}
	if code, out = apiSend(t, e, adm, "GET", fmt.Sprintf("/api/v1/runs/%d/files?path=X", runID), ""); code != 404 {
		t.Errorf("unknown volume: %d %s", code, out)
	}

	// Restoring files from the image: one run per volume.
	code, out = apiPost(t, e, adm, fmt.Sprintf("/api/v1/runs/%d/restore", runID), `{"target":"D:\\Restore","paths":["C/Users/ana","C/Data"]}`)
	var rr struct {
		RunID  int64   `json:"run_id"`
		RunIDs []int64 `json:"run_ids"`
	}
	json.Unmarshal([]byte(out), &rr)
	if code != 202 || rr.RunID == 0 || len(rr.RunIDs) != 1 {
		t.Fatalf("file restore: %d %s", code, out)
	}
	if r, _ := e.store.GetRun(ctx, rr.RunID); r.Kind != api.KindImageFileRestore || len(r.Paths) != 2 || r.Paths[0] != `\Users\ana` {
		t.Errorf("file restore run: %+v", r)
	}
	if code, out = apiPost(t, e, adm, fmt.Sprintf("/api/v1/runs/%d/restore", runID), `{"original":true}`); code != 400 {
		t.Errorf("image file restore without paths: %d %s", code, out)
	}

	// Onto a disk: confirmed, never the system disk, by disk number.
	p := fmt.Sprintf("/api/v1/runs/%d/restore-disk", runID)
	for body, want := range map[string]int{
		`{"disk":0,"target_disk":1}`:                                400, // not confirmed
		`{"disk":0,"confirm_erase":true}`:                           400, // no target disk
		`{"disk":4,"target_disk":1,"confirm_erase":true}`:           400, // not in the backup
		`{"disk":0,"target_disk":0,"confirm_erase":true}`:           409, // the running Windows
		`{"disk":0,"image":0,"target_disk":1,"confirm_erase":true}`: 400,
	} {
		if code, out := apiPost(t, e, adm, p, body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, out, want)
		}
	}
	if code, _ := apiPost(t, e, view, p, `{"disk":0,"target_disk":1,"confirm_erase":true}`); code != 403 {
		t.Errorf("viewer restored a disk: %d", code)
	}
	code, out = apiPost(t, e, adm, p, `{"disk":0,"target_disk":1,"confirm_erase":true,"new_hardware":true,"driver_path":"E:\\Drivers"}`)
	var dr struct {
		RunID int64 `json:"run_id"`
	}
	json.Unmarshal([]byte(out), &dr)
	if code != 202 || dr.RunID == 0 {
		t.Fatalf("disk restore: %d %s", code, out)
	}
	if r, _ := e.store.GetRun(ctx, dr.RunID); r.Kind != api.KindImageRestore || r.TargetDisk == nil || *r.TargetDisk != 1 || !r.NewHardware {
		t.Errorf("disk restore run: %+v", r)
	}
}

// TestAPITargetKeyAndCodes: a target that shares the recovery key of
// another, and listing and revoking enrollment codes.
func TestAPITargetKeyAndCodes(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	adm := apiToken(t, admin, "admin")

	code, out := apiPost(t, e, adm, "/api/v1/targets", fmt.Sprintf(`{"name":"Cloud","kind":"local","url":%q}`, filepath.Join(t.TempDir(), "a")))
	var first struct {
		Target struct {
			ID int64 `json:"id"`
		} `json:"target"`
		RecoveryKey string `json:"recovery_key"`
	}
	json.Unmarshal([]byte(out), &first)
	if code != 202 || first.RecoveryKey == "" {
		t.Fatalf("first target: %d %s", code, out)
	}
	code, out = apiPost(t, e, adm, "/api/v1/targets", fmt.Sprintf(`{"name":"NAS","kind":"local","url":%q,"recovery_key_from":%d}`, filepath.Join(t.TempDir(), "b"), first.Target.ID))
	var second struct {
		Target struct {
			ID int64 `json:"id"`
		} `json:"target"`
		From int64 `json:"recovery_key_from"`
	}
	json.Unmarshal([]byte(out), &second)
	if code != 202 || second.From != first.Target.ID || strings.Contains(out, "recovery_key\"") {
		t.Fatalf("second target: %d %s", code, out)
	}
	if tg, _ := e.store.GetTarget(ctx, second.Target.ID); tg.RecoveryKey != first.RecoveryKey {
		t.Error("the recovery key was not shared")
	}
	plain, _ := e.store.CreateTarget(ctx, server.Target{Name: "plain", Kind: "local", URL: t.TempDir()})
	for _, body := range []string{
		fmt.Sprintf(`{"name":"x","kind":"local","url":"/x","recovery_key_from":%d}`, plain),
		`{"name":"x","kind":"local","url":"/x","recovery_key_from":9999}`,
		fmt.Sprintf(`{"name":"x","kind":"local","url":"/x","encrypted":false,"recovery_key_from":%d}`, first.Target.ID),
	} {
		if code, out := apiPost(t, e, adm, "/api/v1/targets", body); code != 400 {
			t.Errorf("%s: %d %s", body, code, out)
		}
	}

	// Name and credentials change; type, location and key stay.
	smb, err := e.store.CreateTarget(ctx, server.Target{Name: "Office NAS", Kind: "smb", URL: "smb://backup@nas/share", SMBPassword: "old", Encrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	if code, out := apiSend(t, e, adm, "PATCH", fmt.Sprintf("/api/v1/targets/%d", smb), `{"smb_password":"new","name":"Office NAS 2"}`); code != 200 || !strings.Contains(out, `"Office NAS 2"`) || strings.Contains(out, "new") {
		t.Errorf("patch: %d %s", code, out)
	}
	if tg, _ := e.store.GetTarget(ctx, smb); tg.SMBPassword != "new" || tg.Name != "Office NAS 2" || tg.URL != "smb://backup@nas/share" || tg.RecoveryKey == "" {
		t.Errorf("patched target: %+v", tg)
	}
	for body, want := range map[string]int{`{"smb_password":""}`: 400, `{"url":"smb://x@y/z"}`: 400, `{"name":"Cloud"}`: 400} {
		if code, out := apiSend(t, e, adm, "PATCH", fmt.Sprintf("/api/v1/targets/%d", smb), body); code != want {
			t.Errorf("patch %s: %d %s", body, code, out)
		}
	}
	if code, _ := apiSend(t, e, adm, "PATCH", "/api/v1/targets/9999", `{"name":"x"}`); code != 404 {
		t.Errorf("patch unknown target: %d", code)
	}

	// Codes: listed without their secret, revoked by label prefix or id.
	for _, l := range []string{"itdocs:c1:recovery:d1", "itdocs:c1:recovery:d2", "itdocs:c1:d3"} {
		if code, out := apiPost(t, e, adm, "/api/v1/agents/enroll-code", `{"ttl_minutes":60,"label":"`+l+`"}`); code != 202 {
			t.Fatalf("code: %d %s", code, out)
		}
	}
	code, out = apiSend(t, e, adm, "GET", "/api/v1/agents/enroll-codes?label_prefix=itdocs:c1:recovery:", "")
	var codes []struct {
		ID    int64  `json:"id"`
		Label string `json:"label"`
	}
	json.Unmarshal([]byte(out), &codes)
	if code != 200 || len(codes) != 2 || strings.Contains(out, "BZ1-") || strings.Contains(out, "token") {
		t.Fatalf("list: %d %s", code, out)
	}
	if code, out := apiPost(t, e, adm, "/api/v1/agents/enroll-codes/revoke", `{}`); code != 400 {
		t.Errorf("revoke without selection: %d %s", code, out)
	}
	if code, out := apiPost(t, e, adm, "/api/v1/agents/enroll-codes/revoke", `{"label_prefix":"itdocs:c1:recovery:"}`); code != 202 || !strings.Contains(out, `"revoked":2`) {
		t.Errorf("revoke: %d %s", code, out)
	}
	_, out = apiSend(t, e, adm, "GET", "/api/v1/agents/enroll-codes?label_prefix=itdocs:c1:", "")
	json.Unmarshal([]byte(out), &codes)
	if len(codes) != 1 || codes[0].Label != "itdocs:c1:d3" {
		t.Fatalf("after revoke: %s", out)
	}
	if code, out := apiPost(t, e, adm, "/api/v1/agents/enroll-codes/revoke", fmt.Sprintf(`{"id":%d}`, codes[0].ID)); code != 202 || !strings.Contains(out, `"revoked":1`) {
		t.Errorf("revoke by id: %d %s", code, out)
	}
}
