package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/max-zit/backupzit/internal/api"
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
	mux.HandleFunc("POST /api/v1/targets", s.api(PermStorage, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req apiTargetRequest
		if err := decodeJSON(r, &req); err != nil {
			return nil, err
		}
		in := req.targetInput
		in.Encrypted = req.Encrypted == nil || *req.Encrypted
		in.S3Immutable = in.S3LockDays != 0
		t, err := in.target()
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "%s", err.Error())
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
		if saved.Encrypted {
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

// apiTargetTest runs the connection test of the Storage page.
func (s *Server) apiTargetTest(r *http.Request, t Target) map[string]any {
	ok, err := testTarget(r.Context(), t)
	msg, err := targetTestMessage(requestLanguage(r), t.Name, ok, err)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	return map[string]any{"ok": true, "message": msg}
}
