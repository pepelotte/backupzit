package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ---- local and network destinations of jobs

// customerCloudTarget finds the customer's storage in the shared bucket
// (created by target.provision). Its recovery key also encrypts the
// customer's local and network targets, so one key restores everything.
func (c *connector) customerCloudTarget(ctx context.Context, customerID string) (map[string]any, []map[string]any, error) {
	prefix, err := customerPrefix(customerID)
	if err != nil {
		return nil, nil, err
	}
	var targets []map[string]any
	if err := c.bz.call(ctx, http.MethodGet, "/targets", nil, &targets); err != nil {
		return nil, nil, err
	}
	if c.cfg.B2.Bucket != "" {
		for _, t := range targets {
			if loc, _ := t["location"].(string); strings.Contains(loc, "/"+c.cfg.B2.Bucket+"/"+prefix) {
				return t, targets, nil
			}
		}
	}
	return nil, targets, fail("Für diesen Kunden gibt es noch keinen Cloud-Speicher. Bitte zuerst den Backup-Speicher anlegen: sein Wiederherstellungsschlüssel verschlüsselt auch lokale und Netzwerkziele")
}

var winPath = regexp.MustCompile(`^[A-Za-z]:\\`)

type destination struct {
	Type     string `json:"type"`
	Path     string `json:"path"`
	Username string `json:"username"`
	Password string `json:"password"`
	Domain   string `json:"domain"`
}

// smbParts splits \\server\share\folder (also //server/share or
// smb://server/share) into its parts.
func smbParts(p string) (host, share, dir string, ok bool) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "smb:")
	p = strings.ReplaceAll(p, `\`, "/")
	if !strings.HasPrefix(p, "//") {
		return "", "", "", false
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], strings.Join(parts[2:], "/"), true
}

// destinationTarget finds or creates the BackupZit target for a job's
// local or network destination: encrypted with the key of the customer's
// cloud storage, named after it, reused for the same path.
func (c *connector) destinationTarget(ctx context.Context, customerID string, raw json.RawMessage) (map[string]any, bool, []string, error) {
	var d destination
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, false, nil, fail("ungültiges Ziel: %v", err)
	}
	cloud, targets, err := c.customerCloudTarget(ctx, customerID)
	if err != nil {
		return nil, false, nil, err
	}
	cloudName, _ := cloud["name"].(string)
	body := map[string]any{"recovery_key_from": cloud["id"]}
	var name, kind string
	switch d.Type {
	case "local":
		path := strings.TrimSpace(d.Path)
		if !winPath.MatchString(path) && !strings.HasPrefix(path, "/") {
			return nil, false, nil, fail("Der lokale Pfad muss vollständig sein, z. B. E:\\Backups oder /mnt/backup")
		}
		kind, name = "local", cloudName+" · Lokal "+path
		body["url"] = path
	case "network":
		host, share, dir, ok := smbParts(d.Path)
		if !ok {
			return nil, false, nil, fail("Die Netzwerkfreigabe muss als \\\\server\\freigabe\\ordner angegeben werden")
		}
		user, domain := strings.TrimSpace(d.Username), strings.TrimSpace(d.Domain)
		if dom, u, found := strings.Cut(user, `\`); found && domain == "" {
			domain, user = dom, u
		}
		if user == "" || d.Password == "" {
			return nil, false, nil, fail("Für die Netzwerkfreigabe fehlen Benutzername oder Kennwort")
		}
		kind, name = "smb", cloudName+" · NAS \\\\"+host+"\\"+share
		if dir != "" {
			name += "\\" + strings.ReplaceAll(dir, "/", "\\")
		}
		body["smb_host"], body["smb_share"], body["smb_path"], body["smb_user"], body["smb_password"] = host, share, dir, user, d.Password
		if domain != "" {
			body["smb_domain"] = domain
		}
	}
	for _, t := range targets {
		if t["name"] == name && t["kind"] == kind {
			// The same path: reuse it, with the password given now.
			if kind == "smb" {
				upd := map[string]any{"smb_password": d.Password}
				if dom, ok := body["smb_domain"]; ok {
					upd["smb_domain"] = dom
				}
				if err := c.bz.call(ctx, http.MethodPatch, fmt.Sprintf("/targets/%d", toInt(t["id"])), upd, nil); err != nil {
					return nil, false, nil, err
				}
			}
			return t, false, nil, nil
		}
	}
	body["name"], body["kind"] = name, kind
	var res struct {
		Target map[string]any `json:"target"`
		Test   *struct {
			OK bool `json:"ok"`
		} `json:"test"`
	}
	if err := c.bz.call(ctx, http.MethodPost, "/targets", body, &res); err != nil {
		return nil, false, nil, err
	}
	var warn []string
	if res.Test != nil && !res.Test.OK {
		warn = append(warn, "Die Backup-Konsole erreicht die Freigabe nicht selbst (normal im Kundennetz); geprüft wird sie bei der ersten Sicherung durch den Agenten")
	}
	c.log.Info("destination target created", "customer", customerID, "target", toInt(res.Target["id"]), "kind", kind)
	return res.Target, true, warn, nil
}

// ---- image jobs over several disks

type extDisk struct {
	Disk       int   `json:"disk"`
	Partitions []int `json:"partitions"`
	Whole      bool  `json:"whole"`
}

type agentDisk struct {
	Number     int    `json:"number"`
	Bus        string `json:"bus"`
	Partitions []struct {
		Number int `json:"number"`
	} `json:"partitions"`
}

// mapExtDisks turns a selection over disks into BackupZit's image job:
// one disk (whole or some partitions) or all internal disks (-1).
func (c *connector) mapExtDisks(ctx context.Context, body map[string]any, raw json.RawMessage) ([]string, error) {
	var sel []extDisk
	if err := json.Unmarshal(raw, &sel); err != nil {
		return nil, fail("ungültige Datenträgerauswahl: %v", err)
	}
	if len(sel) == 0 {
		return nil, nil
	}
	if len(sel) == 1 {
		body["image_disk"] = sel[0].Disk
		if sel[0].Whole || len(sel[0].Partitions) == 0 {
			delete(body, "image_partitions")
		} else {
			body["image_partitions"] = sel[0].Partitions
		}
		return nil, nil
	}
	body["image_disk"] = -1
	delete(body, "image_partitions")
	id, _ := body["agent_id"].(json.RawMessage)
	var agent struct {
		Disks []agentDisk `json:"disks"`
	}
	all := true
	if err := c.bz.call(ctx, http.MethodGet, "/agents/"+strings.TrimSpace(string(id)), nil, &agent); err == nil {
		chosen := map[int]bool{}
		for _, s := range sel {
			chosen[s.Disk] = true
			all = all && (s.Whole || len(s.Partitions) == 0)
		}
		for _, d := range agent.Disks {
			if d.Bus == "" && !chosen[d.Number] {
				all = false
			}
		}
	}
	if all {
		return nil, nil
	}
	return []string{"Eine Auswahl über mehrere Platten sichert BackupZit als „alle internen Datenträger“ vollständig (konsistent aus einem Snapshot); einzelne Partitionen lassen sich nur bei einer Platte pro Job wählen"}, nil
}

// ---- storage targets

func actTargetUpdate(ctx context.Context, c *connector, cmd Command) (any, error) {
	var p map[string]any
	if err := json.Unmarshal(cmd.Params, &p); err != nil {
		return nil, fail("ungültige Parameter: %v", err)
	}
	id, _ := num(p["target_id"])
	if id <= 0 {
		return nil, fail("target_id fehlt")
	}
	body := map[string]any{}
	for _, k := range []string{"name", "smb_password", "smb_domain", "sftp_password"} {
		if v, ok := p[k].(string); ok {
			body[k] = v
		}
	}
	if len(body) == 0 {
		return nil, fail("nichts zu ändern (name, smb_password, smb_domain)")
	}
	var res map[string]any
	err := c.bz.call(ctx, http.MethodPatch, fmt.Sprintf("/targets/%d", int64(id)), body, &res)
	return res, err
}

// ---- browsing the agent

// actAgentBrowse lists the drives of an agent from the disks it reported.
// BackupZit agents cannot list folders on request, so only the first
// level exists; deeper paths are typed in.
func actAgentBrowse(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[struct {
		AgentID int64  `json:"agent_id"`
		Kind    string `json:"kind"`
		Path    string `json:"path"`
	}](cmd)
	if err == nil {
		err = need("agent_id", p.AgentID)
	}
	if err != nil {
		return nil, err
	}
	if p.Kind == "network" {
		return nil, fail("Netzwerkfreigaben kann BackupZit nicht durchsuchen; bitte den Pfad eingeben (\\\\server\\freigabe\\ordner)")
	}
	if path := strings.Trim(strings.TrimSpace(p.Path), `\/`); path != "" {
		return nil, fail("BackupZit kann auf dem Rechner nur die Laufwerke auflisten, keine Ordner; bitte den Pfad darunter eingeben")
	}
	var agent struct {
		OS    string `json:"os"`
		Disks []struct {
			Number     int    `json:"number"`
			Bus        string `json:"bus"`
			Partitions []struct {
				Size        uint64   `json:"size"`
				Label       string   `json:"label"`
				FileSystem  string   `json:"file_system"`
				MountPoints []string `json:"mount_points"`
			} `json:"partitions"`
		} `json:"disks"`
	}
	if err := c.bz.call(ctx, http.MethodGet, fmt.Sprintf("/agents/%d", p.AgentID), nil, &agent); err != nil {
		return nil, err
	}
	type entry struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		Dir        bool   `json:"dir"`
		Size       uint64 `json:"size,omitempty"`
		Label      string `json:"label,omitempty"`
		FileSystem string `json:"file_system,omitempty"`
		Removable  bool   `json:"removable,omitempty"`
	}
	entries := []entry{}
	seen := map[string]bool{}
	for _, d := range agent.Disks {
		for _, pt := range d.Partitions {
			for _, mp := range pt.MountPoints {
				if mp == "" || seen[mp] {
					continue
				}
				seen[mp] = true
				name := strings.TrimRight(mp, `\`)
				if name == "" {
					name = mp
				}
				entries = append(entries, entry{Name: name, Path: mp, Dir: true, Size: pt.Size, Label: pt.Label, FileSystem: pt.FileSystem, Removable: d.Bus != ""})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return map[string]any{"path": "", "entries": entries, "drives_only": true}, nil
}

// ---- restore

func actRestoreOriginal(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[struct {
		RunID   int64    `json:"run_id"`
		AgentID int64    `json:"agent_id"`
		Paths   []string `json:"paths"`
	}](cmd)
	if err == nil {
		err = need("run_id", p.RunID)
	}
	if err != nil {
		return nil, err
	}
	body := map[string]any{"original": true, "paths": p.Paths}
	if p.AgentID > 0 {
		body["agent_id"] = p.AgentID
	}
	var res map[string]any
	err = c.bz.call(ctx, http.MethodPost, fmt.Sprintf("/runs/%d/restore", p.RunID), body, &res)
	return res, err
}

type diskMapping struct {
	SourceDisk   *int   `json:"source_disk"`
	TargetDisk   *int   `json:"target_disk"`
	TargetDevice string `json:"target_device"`
	Partitions   []int  `json:"partitions"`
}

// actRestoreDisks restores a disk image (or a Linux system backup) onto
// whole disks: restore.machine, and restore.volumes when the chosen
// volumes are all of a disk. The platform has the technician confirm that
// the target disks are erased.
func actRestoreDisks(volumes bool) handler {
	return func(ctx context.Context, c *connector, cmd Command) (any, error) {
		p, err := params[struct {
			RunID       int64         `json:"run_id"`
			AgentID     int64         `json:"agent_id"`
			TargetType  string        `json:"target_type"`
			Mapping     []diskMapping `json:"mapping"`
			NewHardware bool          `json:"new_hardware"`
			DriverPath  string        `json:"driver_path"`
		}](cmd)
		if err == nil {
			err = need("run_id", p.RunID)
		}
		if err != nil {
			return nil, err
		}
		if p.TargetType != "" && p.TargetType != "physical" {
			return nil, fail("Wiederherstellen als virtuelle Maschine geht mit BackupZit über das Wiederherstellungsmedium: die VM davon starten und als physischen Rechner wiederherstellen")
		}
		if len(p.Mapping) == 0 {
			return nil, fail("keine Zuordnung von Quell- zu Zielplatte angegeben")
		}
		var run struct {
			Kind       string `json:"kind"`
			ImageDisks []struct {
				Number     int `json:"number"`
				Partitions []struct {
					Number   int  `json:"number"`
					Included bool `json:"included"`
				} `json:"partitions"`
			} `json:"image_disks"`
		}
		if err := c.bz.call(ctx, http.MethodGet, fmt.Sprintf("/runs/%d", p.RunID), nil, &run); err != nil {
			return nil, err
		}
		var bodies []map[string]any
		for _, m := range p.Mapping {
			b := map[string]any{"confirm_erase": true, "new_hardware": p.NewHardware}
			if p.AgentID > 0 {
				b["agent_id"] = p.AgentID
			}
			switch run.Kind {
			case "image-backup":
				if m.TargetDisk == nil {
					return nil, fail("Zielplatte fehlt")
				}
				src := 0
				if m.SourceDisk != nil {
					src = *m.SourceDisk
				}
				if volumes && len(m.Partitions) > 0 {
					for _, d := range run.ImageDisks {
						if d.Number != src {
							continue
						}
						chosen := map[int]bool{}
						for _, n := range m.Partitions {
							chosen[n] = true
						}
						for _, pt := range d.Partitions {
							if pt.Included && !chosen[pt.Number] {
								return nil, fail("Einzelne Volumes kann BackupZit nicht auf eine Platte zurückschreiben, nur die ganze Platte. Für einzelne Volumes die Dateien wiederherstellen (in einen Ordner oder an den Originalort)")
							}
						}
					}
				}
				b["disk"], b["target_disk"] = src, *m.TargetDisk
				if p.DriverPath != "" {
					b["driver_path"] = p.DriverPath
				}
			case "system-backup":
				if !strings.HasPrefix(m.TargetDevice, "/dev/") {
					return nil, fail("Für ein Linux-System bitte das Zielgerät angeben (target_device, z. B. /dev/sda)")
				}
				if volumes {
					return nil, fail("Ein Linux-System wird immer als ganze Platte wiederhergestellt")
				}
				b["device"] = m.TargetDevice
			default:
				return nil, fail("Lauf %d ist keine Abbild- oder Linux-Systemsicherung", p.RunID)
			}
			bodies = append(bodies, b)
		}
		var ids []int64
		for _, b := range bodies {
			var res struct {
				RunID int64 `json:"run_id"`
			}
			if err := c.bz.call(ctx, http.MethodPost, fmt.Sprintf("/runs/%d/restore-disk", p.RunID), b, &res); err != nil {
				if len(ids) > 0 {
					return nil, fmt.Errorf("nach %d gestarteten Wiederherstellungen (Läufe %v): %w", len(ids), ids, friendly(err))
				}
				return nil, err
			}
			ids = append(ids, res.RunID)
		}
		return map[string]any{"run_id": ids[0], "run_ids": ids}, nil
	}
}

// ---- recovery media

// recoveryLabel is the label of a customer's recovery codes; device ""
// matches all of them.
func recoveryLabel(customerID, deviceID string) string {
	return "itdocs:" + customerID + ":recovery:" + deviceID
}

// actRecoveryCode creates recovery.json for the recovery stick: a code
// that enrolls the machine started from the stick as a recovery agent.
// It is valid for several machines (a restore can need a second boot),
// by default for 24 hours.
func actRecoveryCode(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[struct {
		TTLHours int `json:"ttl_hours"`
	}](cmd)
	if err != nil {
		return nil, err
	}
	if _, err := customerPrefix(cmd.CustomerID); err != nil {
		return nil, err
	}
	if p.TTLHours == 0 {
		p.TTLHours = 24
	}
	if p.TTLHours < 1 || p.TTLHours > 720 {
		return nil, fail("Gültigkeit 1 bis 720 Stunden")
	}
	label := recoveryLabel(cmd.CustomerID, cmd.DeviceID)
	var code struct {
		Code    string `json:"code"`
		Expires string `json:"expires"`
	}
	if err := c.bz.call(ctx, http.MethodPost, "/agents/enroll-code", map[string]any{
		"ttl_minutes": p.TTLHours * 60, "single_use": false, "label": label,
	}, &code); err != nil {
		return nil, err
	}
	rec, err := recoveryJSON(code.Code)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(c.cfg.TempDir, "recovery-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "recovery.json")
	if err := os.WriteFile(file, rec, 0o600); err != nil {
		return nil, err
	}
	path, err := c.pf.upload(ctx, cmd.ID, "recovery.json", file)
	if err != nil {
		return nil, fmt.Errorf("Hochladen zur Plattform: %w", err)
	}
	c.log.Info("recovery code created", "customer", cmd.CustomerID, "device", cmd.DeviceID, "expires", code.Expires)
	return map[string]any{"storage_path": path, "file_name": "recovery.json", "code_expires": code.Expires, "enroll_label": label,
		"expires": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}, nil
}

// recoveryJSON is the file the recovery media reads: the console's address,
// the code's token and the certificate fingerprint, from a BZ1 code.
func recoveryJSON(code string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, "BZ1-"))
	var c struct { // internal/api/enrollcode.go
		Server      string `json:"s"`
		Token       string `json:"t"`
		Fingerprint string `json:"f"`
	}
	if err != nil || !strings.HasPrefix(code, "BZ1-") || json.Unmarshal(raw, &c) != nil || c.Server == "" || c.Token == "" {
		return nil, fmt.Errorf("BackupZit lieferte keinen gültigen Code")
	}
	return json.MarshalIndent(map[string]string{"server": c.Server, "token": c.Token, "fingerprint": c.Fingerprint}, "", "  ")
}

func actRecoveryRevoke(ctx context.Context, c *connector, cmd Command) (any, error) {
	if _, err := customerPrefix(cmd.CustomerID); err != nil {
		return nil, err
	}
	var res map[string]any
	err := c.bz.call(ctx, http.MethodPost, "/agents/enroll-codes/revoke",
		map[string]any{"label_prefix": recoveryLabel(cmd.CustomerID, cmd.DeviceID)}, &res)
	return res, err
}
