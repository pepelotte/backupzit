package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// userError is a failure whose text is meant for the technician in the
// platform (German, like the platform's interface).
type userError struct{ msg string }

func (e *userError) Error() string { return e.msg }

// retryDelay is the pause between connection tests of a new target.
var retryDelay = 5 * time.Second

func fail(format string, a ...any) error { return &userError{fmt.Sprintf(format, a...)} }

// friendly turns errors of the console into texts for the platform.
func friendly(err error) error {
	var ue *userError
	if err == nil || errors.As(err, &ue) {
		return err
	}
	var he *httpError
	if errors.As(err, &he) {
		switch he.Status {
		case http.StatusBadRequest:
			return fail("BackupZit hat den Auftrag abgelehnt: %s", he.Msg)
		case http.StatusNotFound:
			return fail("Nicht gefunden: %s", he.Msg)
		case http.StatusConflict:
			return fail("Nicht möglich: %s", he.Msg)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fail("Der Connector darf das in BackupZit nicht (%s) – API-Token und Rolle von svc-connector prüfen", he.Msg)
		}
	}
	return fmt.Errorf("interner Fehler: %w", err)
}

type handler func(ctx context.Context, c *connector, cmd Command) (any, error)

var actions = map[string]handler{
	"target.provision":  actTargetProvision,
	"target.create":     actTargetCreate,
	"target.test":       actTargetTest,
	"agent.enroll_code": actEnrollCode,
	"agent.log":         actAgentLog,
	"job.create":        actJobCreate,
	"job.update":        actJobUpdate,
	"job.delete":        actJobDelete,
	"job.run":           actJobPost("run"),
	"job.restore_test":  actJobPost("test"),
	"job.enable":        actJobPost("enable"),
	"job.disable":       actJobPost("disable"),
	"run.cancel":        actRunCancel,
	"files.list":        actFilesList,
	"files.download":    actFilesDownload,
	"restore.to_folder": actRestoreFolder,
}

// notYet are actions the platform already sends that need more work in
// BackupZit or the connector.
var notYet = map[string]string{
	"agent.browse":           "Ordner auf dem Agenten durchsuchen",
	"restore.files_original": "Wiederherstellung an den Originalort",
	"restore.machine":        "Wiederherstellung eines ganzen Rechners",
	"restore.volumes":        "Wiederherstellung einzelner Volumes",
}

func (c *connector) run(ctx context.Context, cmd Command) (any, error) {
	if h, ok := actions[cmd.Action]; ok {
		res, err := h(ctx, c, cmd)
		return res, friendly(err)
	}
	if what, ok := notYet[cmd.Action]; ok {
		return nil, fail("%s ist im Connector noch nicht umgesetzt", what)
	}
	return nil, fail("unbekannte Auftragsart %q", cmd.Action)
}

func params[T any](cmd Command) (T, error) {
	var p T
	if len(cmd.Params) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(cmd.Params, &p); err != nil {
		return p, fail("ungültige Parameter: %v", err)
	}
	return p, nil
}

type idParams struct {
	TargetID int64 `json:"target_id"`
	AgentID  int64 `json:"agent_id"`
	JobID    int64 `json:"job_id"`
	RunID    int64 `json:"run_id"`
}

func need(name string, v int64) error {
	if v <= 0 {
		return fail("%s fehlt", name)
	}
	return nil
}

// ---- storage

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// customerPrefix is the customer's folder in the shared bucket: an id,
// never a name, so a leaked key or listing reveals no customer names.
func customerPrefix(customerID string) (string, error) {
	if !uuidRe.MatchString(customerID) {
		return "", fail("ungültige Kunden-ID %q", customerID)
	}
	return "c-" + strings.ToLower(customerID), nil
}

func actTargetProvision(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[struct {
		Name string `json:"name"`
	}](cmd)
	if err != nil {
		return nil, err
	}
	if !c.cfg.b2Ready() {
		return nil, fail("Der Backblaze-Zugang des Connectors ist nicht eingerichtet (b2.key_id, b2.app_key, b2.bucket)")
	}
	prefix, err := customerPrefix(cmd.CustomerID)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = prefix
	}
	// One storage target per customer: a second provision would leave two
	// targets with different recovery keys on the same folder.
	var targets []map[string]any
	if err := c.bz.call(ctx, http.MethodGet, "/targets", nil, &targets); err != nil {
		return nil, err
	}
	for _, t := range targets {
		if loc, _ := t["location"].(string); strings.Contains(loc, "/"+c.cfg.B2.Bucket+"/"+prefix) {
			return nil, fail("Für diesen Kunden gibt es bereits einen Backup-Speicher (BackupZit-Ziel %v, %q)", t["id"], t["name"])
		}
	}
	bucketID, err := c.b2.bucket(ctx, c.cfg.B2.Bucket)
	if err != nil {
		return nil, err
	}
	host, region := c.b2.s3Endpoint()
	if host == "" {
		return nil, fail("Backblaze hat keinen S3-Endpunkt gemeldet")
	}
	key, err := c.b2.createKey(ctx, keyName("bz-"+prefix), bucketID, prefix+"/")
	if err != nil {
		return nil, err
	}
	var res struct {
		Target      map[string]any `json:"target"`
		RecoveryKey string         `json:"recovery_key"`
		Test        *struct {
			OK      bool   `json:"ok"`
			Message string `json:"message"`
		} `json:"test"`
	}
	err = c.bz.call(ctx, http.MethodPost, "/targets", map[string]any{
		"name": name, "kind": "s3", "s3_endpoint": host, "s3_region": region, "s3_bucket": c.cfg.B2.Bucket,
		"s3_prefix": prefix, "s3_access_key": key.ID, "s3_secret_key": key.Secret, "s3_lock_days": c.cfg.B2.LockDays,
	}, &res)
	if err != nil {
		// Leave no orphaned key behind.
		if derr := c.b2.deleteKey(context.WithoutCancel(ctx), key.ID); derr != nil {
			c.log.Error("could not delete the unused customer key", "key", key.ID, "err", derr)
		}
		return nil, err
	}
	id, _ := res.Target["id"].(float64)
	test := res.Test
	// A new Backblaze key can take a few seconds until S3 accepts it.
	for i := 0; i < 6 && test != nil && !test.OK && ctx.Err() == nil; i++ {
		select {
		case <-ctx.Done():
		case <-time.After(retryDelay):
			_ = c.bz.call(ctx, http.MethodPost, fmt.Sprintf("/targets/%d/test", int64(id)), nil, &test)
		}
	}
	c.log.Info("customer storage provisioned", "customer", cmd.CustomerID, "target", int64(id), "prefix", prefix)
	out := map[string]any{"target": res.Target, "recovery_key": res.RecoveryKey, "bucket": c.cfg.B2.Bucket, "prefix": prefix}
	if test != nil {
		out["test"] = test
	}
	return out, nil
}

func actTargetCreate(ctx context.Context, c *connector, cmd Command) (any, error) {
	var p map[string]any
	if err := json.Unmarshal(cmd.Params, &p); err != nil {
		return nil, fail("ungültige Parameter: %v", err)
	}
	allowed := map[string]bool{"name": true, "s3_endpoint": true, "s3_region": true, "s3_bucket": true, "s3_prefix": true,
		"s3_access_key": true, "s3_secret_key": true, "s3_lock_days": true}
	body := map[string]any{"kind": "s3"}
	for k, v := range p {
		if allowed[k] {
			body[k] = v
		}
	}
	var res map[string]any
	err := c.bz.call(ctx, http.MethodPost, "/targets", body, &res)
	return res, err
}

func actTargetTest(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[idParams](cmd)
	if err == nil {
		err = need("target_id", p.TargetID)
	}
	if err != nil {
		return nil, err
	}
	var res map[string]any
	err = c.bz.call(ctx, http.MethodPost, fmt.Sprintf("/targets/%d/test", p.TargetID), nil, &res)
	return res, err
}

// ---- agents

func actEnrollCode(ctx context.Context, c *connector, cmd Command) (any, error) {
	var res map[string]any
	err := c.bz.call(ctx, http.MethodPost, "/agents/enroll-code", nil, &res)
	return res, err
}

func actAgentLog(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[idParams](cmd)
	if err == nil {
		err = need("agent_id", p.AgentID)
	}
	if err != nil {
		return nil, err
	}
	var res map[string]any
	err = c.bz.call(ctx, http.MethodPost, fmt.Sprintf("/agents/%d/log", p.AgentID), nil, &res)
	return res, err
}

// ---- jobs

// jobBody turns the platform's job parameters into what BackupZit takes:
// known fields only; wishes BackupZit cannot fulfil yet come back as
// warnings, or fail the command where ignoring them would be wrong.
func jobBody(raw json.RawMessage, create bool) (map[string]any, []string, error) {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, fail("ungültige Parameter: %v", err)
	}
	var warn []string
	body := map[string]any{}
	keep := []string{"name", "paths", "excludes"}
	if create {
		keep = append(keep, "kind", "agent_id", "target_id", "source_job_id", "image_disk", "image_partitions", "enabled")
	}
	for _, k := range keep {
		if v, ok := p[k]; ok && string(v) != "null" {
			body[k] = v
		}
	}
	if d, ok := p["destination"]; ok && string(d) != "null" {
		var dst struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(d, &dst)
		if dst.Type == "local" || dst.Type == "network" {
			return nil, nil, fail("Lokale und Netzwerk-Ziele werden vom Connector noch nicht angelegt; bitte den Cloud-Speicher des Kunden wählen")
		}
	}
	if v, ok := p["encryption_password"]; ok && string(v) != "null" && string(v) != `""` {
		warn = append(warn, "Ein Kennwort pro Job gibt es in BackupZit nicht: verschlüsselt wird immer mit dem Wiederherstellungsschlüssel des Speichers")
	}
	if v, ok := p["ext_disks"]; ok && string(v) != "null" && string(v) != "[]" {
		warn = append(warn, "Eine Auswahl von Partitionen über mehrere Platten kann BackupZit noch nicht: es werden alle Platten vollständig gesichert")
	}
	if s, ok := p["schedule"]; ok && string(s) != "null" {
		m, ext, err := withoutExt(s)
		if err != nil {
			return nil, nil, err
		}
		if ext {
			warn = append(warn, "Erweiterte Zeitplan-Optionen (Monate, Wochentag im Monat, Zeitraum, beim Systemstart, nachholen, Minuten im Zeitfenster) werden noch nicht umgesetzt")
		}
		body["schedule"] = m
	}
	if r, ok := p["retention"]; ok && string(r) != "null" {
		m, ext, err := withoutExt(r)
		if err != nil {
			return nil, nil, err
		}
		if ext {
			warn = append(warn, "Bereinigung nach Alter wird noch nicht umgesetzt: es gelten die Anzahlen (keep_last, keep_daily …)")
		}
		body["retention"] = m
	}
	if o, ok := p["options"]; ok && string(o) != "null" {
		m, ext, err := withoutExt(o)
		if err != nil {
			return nil, nil, err
		}
		if ext {
			warn = append(warn, "Leistungsfenster und Erst-Seeding per USB werden noch nicht umgesetzt")
		}
		known := map[string]bool{"compression": true, "rpo_hours": true, "max_hours": true, "limit_mbps": true, "limit_from": true,
			"limit_to": true, "window": true, "window_from": true, "window_to": true, "pre_command": true, "post_command": true, "command_minutes": true}
		for k := range m {
			if !known[k] {
				delete(m, k)
			}
		}
		body["options"] = m
	}
	return body, warn, nil
}

// withoutExt decodes an object and removes its "ext" member; ext reports
// whether that held anything.
func withoutExt(raw json.RawMessage) (map[string]any, bool, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false, fail("ungültige Parameter: %v", err)
	}
	e, ok := m["ext"]
	delete(m, "ext")
	if !ok || e == nil {
		return m, false, nil
	}
	if x, isMap := e.(map[string]any); isMap {
		for _, v := range x {
			switch v := v.(type) {
			case nil:
			case bool:
				if v {
					return m, true, nil
				}
			case string:
				if v != "" {
					return m, true, nil
				}
			case []any:
				if len(v) > 0 {
					return m, true, nil
				}
			default:
				return m, true, nil
			}
		}
		return m, false, nil
	}
	return m, true, nil
}

func withWarnings(res map[string]any, warn []string) map[string]any {
	if res == nil {
		res = map[string]any{}
	}
	if len(warn) > 0 {
		res["warnings"] = warn
	}
	return res
}

func actJobCreate(ctx context.Context, c *connector, cmd Command) (any, error) {
	body, warn, err := jobBody(cmd.Params, true)
	if err != nil {
		return nil, err
	}
	var job map[string]any
	if err := c.bz.call(ctx, http.MethodPost, "/jobs", body, &job); err != nil {
		return nil, err
	}
	return withWarnings(map[string]any{"id": job["id"], "job": job}, warn), nil
}

func actJobUpdate(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[idParams](cmd)
	if err == nil {
		err = need("job_id", p.JobID)
	}
	if err != nil {
		return nil, err
	}
	body, warn, err := jobBody(cmd.Params, false)
	if err != nil {
		return nil, err
	}
	var job map[string]any
	if err := c.bz.call(ctx, http.MethodPatch, fmt.Sprintf("/jobs/%d", p.JobID), body, &job); err != nil {
		return nil, err
	}
	return withWarnings(map[string]any{"id": job["id"], "job": job}, warn), nil
}

func actJobDelete(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[idParams](cmd)
	if err == nil {
		err = need("job_id", p.JobID)
	}
	if err != nil {
		return nil, err
	}
	var res map[string]any
	err = c.bz.call(ctx, http.MethodDelete, fmt.Sprintf("/jobs/%d", p.JobID), nil, &res)
	return res, err
}

func actJobPost(op string) handler {
	return func(ctx context.Context, c *connector, cmd Command) (any, error) {
		p, err := params[idParams](cmd)
		if err == nil {
			err = need("job_id", p.JobID)
		}
		if err != nil {
			return nil, err
		}
		var res map[string]any
		err = c.bz.call(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/%s", p.JobID, op), nil, &res)
		return res, err
	}
}

func actRunCancel(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[idParams](cmd)
	if err == nil {
		err = need("run_id", p.RunID)
	}
	if err != nil {
		return nil, err
	}
	var res map[string]any
	err = c.bz.call(ctx, http.MethodPost, fmt.Sprintf("/runs/%d/cancel", p.RunID), nil, &res)
	return res, err
}

// ---- files and restore

func actFilesList(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[struct {
		RunID int64  `json:"run_id"`
		Path  string `json:"path"`
	}](cmd)
	if err == nil {
		err = need("run_id", p.RunID)
	}
	if err != nil {
		return nil, err
	}
	var res map[string]any
	err = c.bz.call(ctx, http.MethodGet, fmt.Sprintf("/runs/%d/files?path=%s", p.RunID, url.QueryEscape(p.Path)), nil, &res)
	return res, err
}

func actFilesDownload(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[struct {
		RunID int64    `json:"run_id"`
		Paths []string `json:"paths"`
	}](cmd)
	if err == nil {
		err = need("run_id", p.RunID)
	}
	if err != nil {
		return nil, err
	}
	if len(p.Paths) == 0 {
		return nil, fail("keine Dateien ausgewählt")
	}
	file, name, size, err := c.bz.download(ctx, p.RunID, p.Paths, c.cfg.TempDir)
	if err != nil {
		return nil, err
	}
	defer os.Remove(file)
	path, err := c.pf.upload(ctx, cmd.ID, name, file)
	if err != nil {
		return nil, fmt.Errorf("Hochladen zur Plattform: %w", err)
	}
	return map[string]any{"storage_path": path, "filename": name, "size": size,
		"expires": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}, nil
}

func actRestoreFolder(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[struct {
		RunID   int64    `json:"run_id"`
		AgentID int64    `json:"agent_id"`
		Target  string   `json:"target"`
		Paths   []string `json:"paths"`
	}](cmd)
	if err == nil {
		err = need("run_id", p.RunID)
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Target) == "" {
		return nil, fail("Zielordner fehlt (Wiederherstellung an den Originalort ist nicht Teil dieses Auftrags)")
	}
	body := map[string]any{"target": p.Target, "paths": p.Paths}
	if p.AgentID > 0 {
		body["agent_id"] = p.AgentID
	}
	var res map[string]any
	err = c.bz.call(ctx, http.MethodPost, fmt.Sprintf("/runs/%d/restore", p.RunID), body, &res)
	return res, err
}
