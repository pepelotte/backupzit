package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/imaging"
)

// REST API endpoints that set BackupZit up: storage targets and agent
// enrollment codes. They use the same code as the console's forms, so a
// target created over the API is checked, stored, tested and audited like
// one created on the Storage page.

// apiTargetJSON is a storage target without its credentials.
func apiTargetJSON(t Target) map[string]any {
	return map[string]any{"id": t.ID, "name": t.Name, "kind": t.Kind, "location": t.URL,
		"encrypted": t.Encrypted, "immutable_days": t.S3LockDays}
}

// apiTargetRequest is the body of POST /api/v1/targets: the fields of the
// form on the Storage page. Backups are encrypted unless "encrypted" is
// false; "s3_lock_days" above 0 turns on S3 Object Lock.
type apiTargetRequest struct {
	targetInput
	Encrypted *bool `json:"encrypted"`
	// RecoveryKeyFrom encrypts the new target with the recovery key of an
	// existing encrypted target, so one key restores both (e.g. a NAS and
	// the cloud copy of the same customer). The key is then not returned.
	RecoveryKeyFrom int64 `json:"recovery_key_from"`
}

// apiDisk and apiPartition describe the disks an agent reported, for image
// jobs: the numbers are the ones a job selects.
type apiDisk struct {
	Number     int            `json:"number"`
	Model      string         `json:"model,omitempty"`
	Size       uint64         `json:"size"`
	Style      string         `json:"style"`
	System     bool           `json:"system,omitempty"`
	Bus        string         `json:"bus,omitempty"`
	Partitions []apiPartition `json:"partitions"`
}

type apiPartition struct {
	Number      int      `json:"number"`
	Size        uint64   `json:"size"`
	Kind        string   `json:"kind,omitempty"`
	Name        string   `json:"name,omitempty"`
	FileSystem  string   `json:"file_system,omitempty"`
	Label       string   `json:"label,omitempty"`
	MountPoints []string `json:"mount_points,omitempty"`
	// Used and UsedAt: the space in use at the newest successful image
	// backup of the partition (agents report sizes only).
	Used   *uint64    `json:"used,omitempty"`
	UsedAt *time.Time `json:"used_at,omitempty"`
}

func toAPIDisks(a Agent) []apiDisk {
	var out []apiDisk
	for _, d := range a.Disks() {
		x := apiDisk{Number: d.Number, Model: d.Model, Size: d.Size, Style: d.Style, System: d.System, Bus: d.Bus, Partitions: []apiPartition{}}
		for _, p := range d.Partitions {
			x.Partitions = append(x.Partitions, apiPartition{Number: p.Number, Size: p.Length, Kind: p.Kind(), Name: p.Name,
				FileSystem: p.FileSystem, Label: p.Label, MountPoints: p.MountPoints})
		}
		out = append(out, x)
	}
	return out
}

// decodeJSON reads a request body into v; unknown fields are an error, so a
// misspelt field is not silently ignored.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return apiErr(http.StatusBadRequest, "a JSON body is required")
		}
		return apiErr(http.StatusBadRequest, "invalid JSON body: %v", err)
	}
	return nil
}

func (s *Server) registerAPIManage(mux *http.ServeMux) {
	s.registerAPIEnrollCodes(mux)
	mux.HandleFunc("POST /api/v1/targets", s.api(PermStorage, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req apiTargetRequest
		if err := decodeJSON(r, &req); err != nil {
			return nil, err
		}
		in := req.targetInput
		in.Encrypted = req.Encrypted == nil || *req.Encrypted
		in.S3Immutable = in.S3LockDays != 0
		if req.RecoveryKeyFrom != 0 && (!in.Encrypted || strings.TrimSpace(in.RecoveryKey) != "") {
			return nil, apiErr(http.StatusBadRequest, "recovery_key_from needs an encrypted target and no recovery_key")
		}
		t, err := in.target()
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "%s", err.Error())
		}
		var from Target
		if req.RecoveryKeyFrom != 0 {
			if from, err = s.store.GetTarget(r.Context(), req.RecoveryKeyFrom); err != nil {
				return nil, apiErr(http.StatusBadRequest, "recovery_key_from: no storage target %d", req.RecoveryKeyFrom)
			}
			if !from.Encrypted || from.RecoveryKey == "" {
				return nil, apiErr(http.StatusBadRequest, "recovery_key_from: storage target %d is not encrypted", from.ID)
			}
			t.RecoveryKey = from.RecoveryKey
		}
		id, err := s.store.CreateTarget(r.Context(), t)
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "%s", err.Error())
		}
		s.audit(r, "storage.create", "%s (%s) %s", t.Name, t.Kind, t.URL)
		saved, err := s.store.GetTarget(r.Context(), id)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"target": apiTargetJSON(saved)}
		// The recovery key is shown once, like the sheet the console offers
		// after creating a target: without it the backups cannot be read
		// if the console is lost.
		if saved.Encrypted && req.RecoveryKeyFrom != 0 {
			out["recovery_key_from"] = from.ID
			s.audit(r, "storage.recovery_key", "%s uses the recovery key of %s", saved.Name, from.Name)
		} else if saved.Encrypted {
			out["recovery_key"] = saved.RecoveryKey
			s.audit(r, "storage.recovery_key", "recovery key of %s returned to the API client that created it", saved.Name)
		}
		// Test it right away, like the console, so a typo shows now and not
		// at the first backup. A failed test leaves the target in place.
		if saved.Kind != "local" && saved.Kind != "usb" {
			out["test"] = s.apiTargetTest(r, saved)
		}
		return out, nil
	}))
	mux.HandleFunc("POST /api/v1/targets/{id}/test", s.api(PermStorage, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		t, err := s.store.GetTarget(r.Context(), id)
		if err != nil {
			return nil, err
		}
		return s.apiTargetTest(r, t), nil
	}))
	mux.HandleFunc("POST /api/v1/agents/enroll-code", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		// Optional body: {"ttl_minutes": 120, "single_use": true, "label": "…"}.
		var req struct {
			TTLMinutes int    `json:"ttl_minutes"`
			SingleUse  bool   `json:"single_use"`
			Label      string `json:"label"`
		}
		if r.ContentLength != 0 {
			if err := decodeJSON(r, &req); err != nil {
				return nil, err
			}
		}
		ttl := 7 * 24 * time.Hour
		if req.TTLMinutes != 0 {
			if req.TTLMinutes < 5 || req.TTLMinutes > 30*24*60 {
				return nil, apiErr(http.StatusBadRequest, "ttl_minutes must be 5 to 43200 (30 days)")
			}
			ttl = time.Duration(req.TTLMinutes) * time.Minute
		}
		label := strings.TrimSpace(req.Label)
		if len(label) > 200 || strings.ContainsFunc(label, unicode.IsControl) {
			return nil, apiErr(http.StatusBadRequest, "label: at most 200 characters, no control characters")
		}
		tok, exp, err := s.store.CreateEnrollmentTokenWith(r.Context(), ttl, req.SingleUse, label)
		if err != nil {
			return nil, err
		}
		pub := s.publicURL(r)
		s.audit(r, "agent.enroll_code", "enrollment code created, valid until %s, single use %v, label %q", exp.Format(time.RFC3339), req.SingleUse, label)
		return map[string]any{"code": api.EncodeEnrollCode(pub, tok, s.CertFingerprint), "expires": exp,
			"server_url": pub, "fingerprint": s.CertFingerprint, "single_use": req.SingleUse, "label": label}, nil
	}))
}

// registerAPIEnrollCodes lists and revokes enrollment codes (also the
// recovery codes on recovery media): by label prefix, or one by id.
func (s *Server) registerAPIEnrollCodes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents/enroll-codes", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		codes, err := s.store.ListEnrollmentCodes(r.Context(), r.URL.Query().Get("label_prefix"))
		if err != nil {
			return nil, err
		}
		if codes == nil {
			codes = []EnrollmentCode{}
		}
		return codes, nil
	}))
	mux.HandleFunc("POST /api/v1/agents/enroll-codes/revoke", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req struct {
			ID          int64  `json:"id"`
			LabelPrefix string `json:"label_prefix"`
		}
		if err := decodeJSON(r, &req); err != nil {
			return nil, err
		}
		if (req.ID > 0) == (req.LabelPrefix != "") || req.ID < 0 {
			return nil, apiErr(http.StatusBadRequest, "give either id or label_prefix")
		}
		n, err := s.store.RevokeEnrollmentCodes(r.Context(), req.LabelPrefix, req.ID)
		if err != nil {
			return nil, err
		}
		s.audit(r, "agent.enroll_code", "%d enrollment codes revoked (id %d, label prefix %q)", n, req.ID, req.LabelPrefix)
		return map[string]int64{"revoked": n}, nil
	}))
}

// apiTargetTest runs the connection test of the Storage page.
func (s *Server) apiTargetTest(r *http.Request, t Target) map[string]any {
	ok, err := testTarget(r.Context(), t)
	msg, err := targetTestMessage(requestLanguage(r), t.Name, ok, err)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	return map[string]any{"ok": true, "message": msg}
}

// addPartitionUsage fills Used of the partitions from the newest
// successful image backups of the agent: the backup reads only the blocks
// in use, so their size is the space in use at that time. Disks are matched
// by their GPT ID or MBR signature (numbers can change), else by number.
func (s *Server) addPartitionUsage(ctx context.Context, agentID int64, disks []apiDisk, inv []imaging.Disk) {
	if len(disks) == 0 {
		return
	}
	rows, err := s.store.db.Query(ctx, `SELECT details, finished_at FROM runs
		WHERE agent_id=$1 AND kind='image-backup' AND status IN ('success','warning') AND details IS NOT NULL AND NOT expired
		ORDER BY finished_at DESC LIMIT 20`, agentID)
	if err != nil {
		return
	}
	type part struct {
		used uint64
		at   time.Time
	}
	found := map[string]part{} // disk key + "/" + partition number, newest first wins
	for rows.Next() {
		var details []byte
		var at *time.Time
		if rows.Scan(&details, &at) != nil || at == nil {
			continue
		}
		for _, d := range imageDetails(Run{Kind: api.KindImageBackup, Details: details}) {
			for _, p := range d.Partitions {
				if !p.Included || p.StoredBytes == 0 {
					continue
				}
				k := diskKey(d.GPTDiskID, d.MBRSignature, d.Number) + "/" + strconv.Itoa(p.Number)
				if _, ok := found[k]; !ok {
					found[k] = part{p.StoredBytes, *at}
				}
			}
		}
	}
	rows.Close()
	for i := range disks {
		var key string
		for _, d := range inv {
			if d.Number == disks[i].Number {
				key = diskKey(d.GPTDiskID, d.MBRSignature, d.Number)
			}
		}
		for j := range disks[i].Partitions {
			p := &disks[i].Partitions[j]
			if f, ok := found[key+"/"+strconv.Itoa(p.Number)]; ok && f.used <= p.Size {
				p.Used, p.UsedAt = &f.used, &f.at
			}
		}
	}
}

func diskKey(gptID string, mbr uint32, number int) string {
	switch {
	case gptID != "":
		return "gpt:" + strings.ToLower(gptID)
	case mbr != 0:
		return "mbr:" + strconv.FormatUint(uint64(mbr), 16)
	}
	return "n:" + strconv.Itoa(number)
}
