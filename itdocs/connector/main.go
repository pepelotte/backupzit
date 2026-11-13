// Command backupzit-connector links the itdocs management platform with
// the BackupZit console on the same server. It only connects outwards:
// it fetches commands from the platform, carries them out with the
// console's local REST API, and reports results and the current state
// back. Nothing listens on a port.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

type connector struct {
	cfg *Config
	bz  *backupZit
	pf  *platform
	b2  *b2
	log *slog.Logger

	mu       sync.Mutex
	runsSent map[int64]string // run id -> status|finished, to report changes once
}

func newConnector(cfg *Config, log *slog.Logger) *connector {
	return &connector{
		cfg: cfg, log: log,
		bz:       newBackupZit(cfg.BackupZit.URL, cfg.BackupZit.Token, cfg.BackupZit.Fingerprint),
		pf:       newPlatform(cfg.Platform.URL, cfg.Platform.Token),
		b2:       newB2(cfg.B2.KeyID, cfg.B2.AppKey),
		runsSent: map[int64]string{},
	}
}

func main() {
	cfgPath := flag.String("config", "/etc/backupzit-connector/config.json", "configuration file")
	check := flag.Bool("check", false, "test the connections to the console, the platform and Backblaze, then exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("backupzit-connector", version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backupzit-connector:", err)
		os.Exit(1)
	}
	c := newConnector(cfg, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *check {
		if !c.check(ctx) {
			os.Exit(1)
		}
		return
	}
	log.Info("backupzit-connector started", "version", version, "platform", cfg.Platform.URL, "console", cfg.BackupZit.URL,
		"b2_bucket", cfg.B2.Bucket, "provisioning", cfg.b2Ready())
	c.loop(ctx)
	log.Info("backupzit-connector stopped")
}

// check prints one line per connection; it returns false if one fails.
func (c *connector) check(ctx context.Context) bool {
	ok := true
	report := func(what string, err error, detail string) {
		if err != nil {
			ok = false
			fmt.Printf("FEHLER  %-28s %v\n", what, err)
			return
		}
		fmt.Printf("ok      %-28s %s\n", what, detail)
	}
	var st map[string]any
	err := c.bz.call(ctx, http.MethodGet, "/status", nil, &st)
	report("BackupZit-Konsole", err, fmt.Sprintf("Version %v, %v Agenten", st["version"], st["agents"]))
	err = c.pf.status(ctx, map[string]any{})
	report("Plattform (Token, IP-Freigabe)", err, c.cfg.Platform.URL)
	if c.cfg.b2Ready() {
		id, err := c.b2.bucket(ctx, c.cfg.B2.Bucket)
		host, region := c.b2.s3Endpoint()
		report("Backblaze (Bucket, Object Lock)", err, fmt.Sprintf("%s (%s), S3 %s, Region %s", c.cfg.B2.Bucket, id, host, region))
	} else {
		fmt.Printf("–       %-28s nicht eingerichtet: Speicher kann nicht automatisch angelegt werden\n", "Backblaze")
	}
	return ok
}

func (c *connector) loop(ctx context.Context) {
	poll := time.NewTicker(c.cfg.pollEvery())
	defer poll.Stop()
	full := time.NewTicker(c.cfg.statusEvery())
	defer full.Stop()
	runs := time.NewTicker(15 * time.Second)
	defer runs.Stop()
	c.syncStatus(ctx)
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			if err := c.processCommands(ctx); err != nil {
				failures++
				// Log the first failure and then every 60th, not every 5 seconds.
				if failures == 1 || failures%60 == 0 {
					c.log.Error("fetching commands failed", "err", err, "failures", failures)
				}
			} else if failures > 0 {
				c.log.Info("platform reachable again", "after_failures", failures)
				failures = 0
			}
		case <-full.C:
			c.syncStatus(ctx)
		case <-runs.C:
			c.syncRecentRuns(ctx)
		}
	}
}

// processCommands claims and carries out commands one by one.
func (c *connector) processCommands(ctx context.Context) error {
	cmds, err := c.pf.claim(ctx, 5)
	if err != nil {
		return err
	}
	for _, cmd := range cmds {
		// The platform gives a claimed command 10 minutes.
		cctx, cancel := context.WithTimeout(ctx, 9*time.Minute)
		start := time.Now()
		res, err := c.run(cctx, cmd)
		cancel()
		var ue *userError
		switch {
		case err == nil:
			c.log.Info("command done", "id", cmd.ID, "action", cmd.Action, "customer", cmd.CustomerID, "took", time.Since(start).Round(time.Millisecond))
		case errors.As(err, &ue):
			c.log.Warn("command failed", "id", cmd.ID, "action", cmd.Action, "customer", cmd.CustomerID, "err", err)
		default:
			c.log.Error("command failed", "id", cmd.ID, "action", cmd.Action, "customer", cmd.CustomerID, "err", err)
		}
		if cerr := c.pf.complete(context.WithoutCancel(ctx), cmd.ID, res, err); cerr != nil {
			c.log.Error("reporting the result failed", "id", cmd.ID, "err", cerr)
		}
		if err == nil && (cmd.Action == "job.run" || cmd.Action == "job.restore_test" || cmd.Action == "restore.to_folder") {
			c.syncRecentRuns(ctx) // show the queued run at once
		}
	}
	return nil
}

// ---- state

func (c *connector) syncStatus(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	st, err := c.collect(ctx)
	if err != nil {
		c.log.Error("reading the console state failed", "err", err)
		return
	}
	if err := c.pf.status(ctx, st); err != nil {
		c.log.Error("reporting the state failed", "err", err)
		return
	}
	c.mu.Lock()
	for _, r := range st["runs"].([]map[string]any) {
		c.runsSent[toInt(r["id"])] = runKey(r)
	}
	c.mu.Unlock()
}

func (c *connector) collect(ctx context.Context) (map[string]any, error) {
	var agents, targets, jobs, storage, runs []map[string]any
	if err := c.bz.call(ctx, http.MethodGet, "/agents", nil, &agents); err != nil {
		return nil, err
	}
	for i, a := range agents {
		var full map[string]any
		if err := c.bz.call(ctx, http.MethodGet, fmt.Sprintf("/agents/%d", toInt(a["id"])), nil, &full); err == nil {
			agents[i] = full
		}
	}
	if err := c.bz.call(ctx, http.MethodGet, "/targets", nil, &targets); err != nil {
		return nil, err
	}
	if err := c.bz.call(ctx, http.MethodGet, "/storage", nil, &storage); err != nil {
		return nil, err
	}
	used := map[int64]any{}
	for _, s := range storage {
		used[toInt(s["target_id"])] = s["used_bytes"]
	}
	for _, t := range targets {
		t["used_bytes"] = used[toInt(t["id"])]
		t["quota_bytes"] = nil
	}
	if err := c.bz.call(ctx, http.MethodGet, "/jobs", nil, &jobs); err != nil {
		return nil, err
	}
	if err := c.bz.call(ctx, http.MethodGet, "/runs?limit=1000", nil, &runs); err != nil {
		return nil, err
	}
	since := time.Now().AddDate(0, 0, -c.cfg.RunDays)
	recent := []map[string]any{}
	for _, r := range runs {
		if t, err := time.Parse(time.RFC3339, fmt.Sprint(r["queued_at"])); err == nil && t.Before(since) {
			continue
		}
		runProgress(r)
		recent = append(recent, r)
	}
	return map[string]any{"agents": cap5000(agents), "targets": cap5000(targets), "jobs": cap5000(jobs), "runs": cap5000(recent)}, nil
}

// syncRecentRuns reports runs whose status changed since the last report,
// so a finished backup shows within seconds.
func (c *connector) syncRecentRuns(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var runs []map[string]any
	if err := c.bz.call(ctx, http.MethodGet, "/runs?limit=50", nil, &runs); err != nil {
		return
	}
	var changed []map[string]any
	since := time.Now().AddDate(0, 0, -c.cfg.RunDays)
	c.mu.Lock()
	for _, r := range runs {
		if t, err := time.Parse(time.RFC3339, fmt.Sprint(r["queued_at"])); err == nil && t.Before(since) {
			continue
		}
		runProgress(r)
		if c.runsSent[toInt(r["id"])] != runKey(r) {
			changed = append(changed, r)
		}
	}
	c.mu.Unlock()
	if len(changed) == 0 {
		return
	}
	if err := c.pf.status(ctx, map[string]any{"runs": changed}); err != nil {
		c.log.Error("reporting runs failed", "err", err)
		return
	}
	c.mu.Lock()
	for _, r := range changed {
		c.runsSent[toInt(r["id"])] = runKey(r)
	}
	c.mu.Unlock()
}

// runKey changes when a run must be sent again: a new status, the end, or
// new progress while it runs.
func runKey(r map[string]any) string {
	k := fmt.Sprint(r["status"], "|", r["finished_at"])
	if st, ok := r["stats"].(map[string]any); ok && r["status"] == "running" {
		k += fmt.Sprint("|", st["bytes_done"], "|", st["files_done"])
	}
	return k
}

// runProgress puts the progress of a running run into stats, in the
// platform's fields. BackupZit agents report processed bytes, not
// uploaded ones, so bytes_uploaded is absent and speed_bps is the
// processing speed.
func runProgress(r map[string]any) {
	p, ok := r["progress"].(map[string]any)
	delete(r, "progress")
	if !ok || r["status"] != "running" {
		return
	}
	st := map[string]any{}
	for from, to := range map[string]string{"bytes_done": "bytes_done", "bytes_total": "bytes_total", "files_done": "files_done",
		"percent": "percent", "eta_s": "eta_s", "speed_bps": "speed_bps", "estimated": "percent_estimated", "reported_at": "reported_at"} {
		if v, ok := p[from]; ok {
			st[to] = v
		}
	}
	r["stats"] = st
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case int64:
		return x
	case int:
		return int64(x)
	}
	return 0
}

func cap5000(l []map[string]any) []map[string]any {
	if l == nil {
		return []map[string]any{}
	}
	if len(l) > 5000 {
		return l[:5000]
	}
	return l
}
