package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// REST API endpoints that create, change and delete backup jobs. The store
// checks a job the same way for the console's forms and for the API
// (checkJob); the API only maps its JSON onto a Job.

// apiJobKinds are the kinds the API creates. Jobs for virtual machines,
// databases, NAS shares and Microsoft 365 need choices that are made in
// the console (guests, engines, sign-in to the tenant).
var apiJobKinds = map[string]bool{JobFiles: true, JobImage: true, JobSystem: true, JobCopy: true}

// apiJobOptions are the options of a job the API can set; a missing field
// keeps the current value (on creation: the default).
type apiJobOptions struct {
	Compression *string `json:"compression"`
	RPOHours    *int    `json:"rpo_hours"`
	MaxHours    *int    `json:"max_hours"`
	LimitMBps   *int    `json:"limit_mbps"`
	LimitFrom   *int    `json:"limit_from"`
	LimitTo     *int    `json:"limit_to"`
	Window      *bool   `json:"window"`
	WindowFrom  *int    `json:"window_from"`
	WindowTo    *int    `json:"window_to"`
	// Commands on the agent before and after a backup need the
	// administrator role, like in the console.
	PreCommand     *string `json:"pre_command"`
	PostCommand    *string `json:"post_command"`
	CommandMinutes *int    `json:"command_minutes"`
}

// apiJobOptionsOut are the options shown in GET /api/v1/jobs; commands are
// only reported as present.
type apiJobOptionsOut struct {
	Compression string `json:"compression,omitempty"`
	RPOHours    int    `json:"rpo_hours,omitempty"`
	MaxHours    int    `json:"max_hours,omitempty"`
	LimitMBps   int    `json:"limit_mbps,omitempty"`
	LimitFrom   int    `json:"limit_from,omitempty"`
	LimitTo     int    `json:"limit_to,omitempty"`
	Window      bool   `json:"window,omitempty"`
	WindowFrom  int    `json:"window_from,omitempty"`
	WindowTo    int    `json:"window_to,omitempty"`
	HasCommands bool   `json:"has_commands,omitempty"`
}

func toAPIJobOptions(o JobOptions) *apiJobOptionsOut {
	return &apiJobOptionsOut{Compression: o.Compression, RPOHours: o.RPOHours, MaxHours: o.MaxHours, LimitMBps: o.LimitMBps,
		LimitFrom: o.LimitFrom, LimitTo: o.LimitTo, Window: o.Window, WindowFrom: o.WindowFrom, WindowTo: o.WindowTo,
		HasCommands: o.HasCommands()}
}

// apply sets the given options on o, normalized like the job form.
func (in *apiJobOptions) apply(o *JobOptions, mayCommands bool) error {
	if in == nil {
		return nil
	}
	if (in.PreCommand != nil || in.PostCommand != nil || in.CommandMinutes != nil) && !mayCommands {
		return apiErr(http.StatusForbidden, "commands before and after a backup need the administrator role")
	}
	set := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	if in.Compression != nil {
		o.Compression = *in.Compression
	}
	set(&o.RPOHours, in.RPOHours)
	set(&o.MaxHours, in.MaxHours)
	set(&o.LimitMBps, in.LimitMBps)
	set(&o.LimitFrom, in.LimitFrom)
	set(&o.LimitTo, in.LimitTo)
	if o.LimitTo == 24 {
		o.LimitTo = 0
	}
	if o.LimitMBps == 0 {
		o.LimitFrom, o.LimitTo = 0, 0
	}
	if in.Window != nil {
		o.Window = *in.Window
	}
	set(&o.WindowFrom, in.WindowFrom)
	set(&o.WindowTo, in.WindowTo)
	if in.PreCommand != nil {
		o.PreCommand = strings.TrimSpace(strings.ReplaceAll(*in.PreCommand, "\r\n", "\n"))
	}
	if in.PostCommand != nil {
		o.PostCommand = strings.TrimSpace(strings.ReplaceAll(*in.PostCommand, "\r\n", "\n"))
	}
	set(&o.CommandMinutes, in.CommandMinutes)
	if o.CommandMinutes == 30 || !o.HasCommands() {
		o.CommandMinutes = 0
	}
	return nil
}

// apiJobCreate is the body of POST /api/v1/jobs.
type apiJobCreate struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	AgentID  int64    `json:"agent_id"`
	TargetID int64    `json:"target_id"`
	SourceID *int64   `json:"source_job_id"` // copy jobs
	Paths    []string `json:"paths"`
	Excludes []string `json:"excludes"`
	// ImageDisk is the disk number of an image job, -1 for all disks;
	// ImagePartitions limits it to some partitions of that disk.
	ImageDisk       *int                  `json:"image_disk"`
	ImagePartitions []int                 `json:"image_partitions"`
	Schedule        *Schedule             `json:"schedule"`
	Retention       *repo.RetentionPolicy `json:"retention"`
	Options         *apiJobOptions        `json:"options"`
	Enabled         *bool                 `json:"enabled"`
}

// apiJobUpdate is the body of PATCH /api/v1/jobs/{id}: what a job may
// change after it was created (like the console's Edit page). Missing
// fields stay as they are.
type apiJobUpdate struct {
	Name      *string               `json:"name"`
	Paths     *[]string             `json:"paths"`
	Excludes  *[]string             `json:"excludes"`
	Schedule  *Schedule             `json:"schedule"`
	Retention *repo.RetentionPolicy `json:"retention"`
	Options   *apiJobOptions        `json:"options"`
}

func encodeAPISchedule(sc *Schedule) string {
	if sc == nil {
		return Schedule{Kind: SchedManual}.Encode()
	}
	return sc.Encode()
}

func (s *Server) registerAPIJobs(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/jobs", s.api(PermJobs, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req apiJobCreate
		if err := decodeJSON(r, &req); err != nil {
			return nil, err
		}
		if !apiJobKinds[req.Kind] {
			return nil, apiErr(http.StatusBadRequest, "kind must be files, image, system or copy; create other jobs in the console")
		}
		job := Job{Kind: req.Kind, Name: req.Name, AgentID: req.AgentID, TargetID: req.TargetID, SourceJobID: req.SourceID,
			Paths: req.Paths, Excludes: req.Excludes, Schedule: encodeAPISchedule(req.Schedule), Retention: defaultRetention, Enabled: true}
		if req.Retention != nil {
			job.Retention = *req.Retention
		}
		if req.Enabled != nil {
			job.Enabled = *req.Enabled
		}
		if err := req.Options.apply(&job.Options, canCommands(r)); err != nil {
			return nil, err
		}
		switch {
		case job.Kind == JobImage && req.ImageDisk != nil:
			d := *req.ImageDisk
			job.ImageDisk = &d
			if d == imaging.AllDisks && len(req.ImagePartitions) > 0 {
				return nil, apiErr(http.StatusBadRequest, "image_partitions need one disk, not all disks (-1)")
			}
			if d != imaging.AllDisks {
				job.ImagePartitions = s.wholeDiskIsNil(r.Context(), job.AgentID, d, req.ImagePartitions)
			}
		case job.Kind != JobImage && (req.ImageDisk != nil || len(req.ImagePartitions) > 0):
			return nil, apiErr(http.StatusBadRequest, "image_disk and image_partitions are only for image jobs")
		}
		if job.Kind != JobCopy && req.SourceID != nil {
			return nil, apiErr(http.StatusBadRequest, "source_job_id is only for copy jobs")
		}
		id, err := s.store.CreateJob(r.Context(), job)
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "%s", err.Error())
		}
		s.audit(r, "job.create", "#%d %s%s", id, job.Name, commandsAudit(JobOptions{}, job.Options))
		saved, err := s.store.GetJob(r.Context(), id)
		if err != nil {
			return nil, err
		}
		return toAPIJob(saved), nil
	}))
	mux.HandleFunc("PATCH /api/v1/jobs/{id}", s.api(PermJobs, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		var req apiJobUpdate
		if err := decodeJSON(r, &req); err != nil {
			return nil, err
		}
		old, err := s.store.GetJob(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if err := apiNotInGroup(old); err != nil {
			return nil, err
		}
		j := old
		if req.Name != nil {
			j.Name = *req.Name
		}
		if req.Paths != nil {
			j.Paths = *req.Paths
		}
		if req.Excludes != nil {
			j.Excludes = *req.Excludes
		}
		if req.Schedule != nil {
			j.Schedule = encodeAPISchedule(req.Schedule)
		}
		if req.Retention != nil {
			j.Retention = *req.Retention
		}
		if err := req.Options.apply(&j.Options, canCommands(r)); err != nil {
			return nil, err
		}
		// With four-eyes approval a shorter retention needs a second user,
		// which only the console can ask for.
		if s.store.fourEyes(r.Context()) && shorterRetention(old.Retention, j.Retention) {
			return nil, apiErr(http.StatusConflict, "four-eyes approval is on: shorten the retention in the console, where a second user approves it")
		}
		if err := s.store.UpdateJob(r.Context(), j); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, err
			}
			return nil, apiErr(http.StatusBadRequest, "%s", err.Error())
		}
		s.audit(r, "job.update", "#%d %s%s", id, j.Name, commandsAudit(old.Options, j.Options))
		saved, err := s.store.GetJob(r.Context(), id)
		if err != nil {
			return nil, err
		}
		return toAPIJob(saved), nil
	}))
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", s.api(PermJobs, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		j, err := s.store.GetJob(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if err := apiNotInGroup(j); err != nil {
			return nil, err
		}
		if s.store.fourEyes(r.Context()) {
			return nil, apiErr(http.StatusConflict, "four-eyes approval is on: delete the job in the console, where a second user approves it")
		}
		if err := s.store.DeleteJob(r.Context(), id); err != nil {
			return nil, err
		}
		s.audit(r, "job.delete", "job %d", id)
		return map[string]any{"id": id, "deleted": true, "note": "existing backups remain on the storage target"}, nil
	}))
}

// apiNotInGroup refuses changes to a job that belongs to a job for several
// agents: that is changed as a whole in the console.
func apiNotInGroup(j Job) error {
	if j.GroupID == nil {
		return nil
	}
	return apiErr(http.StatusConflict, "job %d is part of the job for several agents %q: change it in the console", j.ID, deref(j.GroupName))
}
