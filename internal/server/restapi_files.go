package server

import (
	"errors"

	"github.com/max-zit/backupzit/internal/api"
	"net/http"
	"strings"
	"time"
)

// REST API endpoints that browse and download the files of a backup, like
// the console's file browser: the console reads the snapshot from the
// storage and streams it; nothing is written to the console's disk.

type apiFileEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"` // for "path" and the download, components joined with "/"
	Dir      bool      `json:"dir"`
	Size     uint64    `json:"size"`
	Modified time.Time `json:"modified"`
}

func (s *Server) registerAPIFiles(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/runs/{id}/files", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		run, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if run.Kind == api.KindImageBackup && run.SnapshotID != "" && !run.Expired {
			return s.imageFileList(r.Context(), run, r.URL.Query().Get("path"))
		}
		dir := strings.Join(snapshotComps(r.URL.Query().Get("path")), "/")
		repoR, sn, err := s.fileSnapshot(r.Context(), run)
		if err != nil {
			if errors.Is(err, ErrBrowseLocal) {
				err = errors.New("this backup is on a disk of an agent; the console cannot read it — restore it with POST /api/v1/runs/{id}/restore")
			}
			return nil, apiErr(http.StatusConflict, "%s", err.Error())
		}
		entries, err := listDir(r.Context(), repoR, sn, dir)
		if err != nil {
			return nil, apiErr(http.StatusNotFound, "%s", err.Error())
		}
		out := make([]apiFileEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, apiFileEntry{Name: e.Name, Path: e.Path, Dir: e.IsDir, Size: e.Size, Modified: e.ModTime})
		}
		return map[string]any{"run_id": run.ID, "path": dir, "windows": s.runIsWindows(r.Context(), run),
			"display_path": agentSyntaxPath(snapshotComps(dir), s.runIsWindows(r.Context(), run)), "entries": out}, nil
	}))
	// The download is not JSON: one file as is, several or folders as a
	// ZIP archive. Errors before the first byte are JSON.
	mux.HandleFunc("POST /api/v1/runs/{id}/files/download", func(w http.ResponseWriter, r *http.Request) {
		r, ok := s.apiAuth(PermRestore, w, r)
		if !ok {
			return
		}
		fail := func(code int, msg string) { writeJSON(w, code, map[string]string{"error": msg}) }
		id, err := pathInt(r, "id")
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		run, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			fail(http.StatusNotFound, "not found")
			return
		}
		var req struct {
			Paths []string `json:"paths"`
		}
		if err := decodeJSON(r, &req); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if len(req.Paths) == 0 {
			fail(http.StatusBadRequest, `give the files and folders in "paths", as GET /api/v1/runs/{id}/files lists them`)
			return
		}
		download := s.downloadFiles
		if run.Kind == api.KindImageBackup && run.SnapshotID != "" && !run.Expired {
			download = s.downloadImageFiles
		}
		if err := download(w, r, run, req.Paths); err != nil {
			var nib notInBackupError
			switch {
			case errors.As(err, &nib):
				fail(http.StatusNotFound, err.Error())
			case errors.Is(err, ErrBrowseLocal):
				fail(http.StatusConflict, "this backup is on a disk of an agent; the console cannot read it")
			default:
				fail(http.StatusConflict, err.Error())
			}
		}
	})
}
