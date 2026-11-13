package server

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/imaging"
)

// REST API for disk image backups: the disks of a backup, restoring an
// image onto a disk, and the files inside the image. The files are shown
// like those of a file backup: the first folder is the volume ("C" for
// C:\, or "Disk 1 Partition 2" for a volume without a drive letter), so
// GET /runs/{id}/files, the download and POST /runs/{id}/restore work the
// same for both kinds of backup.

type apiImageDisk struct {
	Index      int                 `json:"index"` // position in the backup ("image" of restore-disk)
	Number     int                 `json:"number"`
	Model      string              `json:"model,omitempty"`
	Size       uint64              `json:"size"`
	Style      string              `json:"style"`
	Partitions []apiImagePartition `json:"partitions"`
}

type apiImagePartition struct {
	Number      int      `json:"number"`
	Size        uint64   `json:"size"`
	FileSystem  string   `json:"file_system,omitempty"`
	Label       string   `json:"label,omitempty"`
	MountPoints []string `json:"mount_points,omitempty"`
	// Included: the contents were backed up (not only the layout).
	Included bool `json:"included"`
	// Used is the size of the blocks that held data at the backup.
	Used uint64 `json:"used,omitempty"`
	// Folder is the first folder of this volume in the file list, for
	// volumes whose files can be browsed (NTFS).
	Folder string `json:"folder,omitempty"`
}

// imageVolume is one browsable volume of an image backup.
type imageVol struct {
	Name   string // first folder in the API's file paths
	Ref    int    // imaging.PartRef
	Letter string // "C" or ""
}

// imageVolumes lists the browsable (NTFS) volumes of an image backup run,
// named by drive letter where there is one.
func imageVolumes(run Run) []imageVol {
	var out []imageVol
	seen := map[string]bool{}
	disks := imageDetails(run)
	for _, d := range disks {
		for _, p := range d.Partitions {
			if !p.Included || p.FileSystem != "NTFS" {
				continue
			}
			v := imageVol{Ref: imaging.PartRef(d.Index, p.Number)}
			for _, m := range p.MountPoints {
				if len(m) >= 2 && m[1] == ':' && (len(m) == 2 || m[2:] == `\`) {
					v.Letter = strings.ToUpper(m[:1])
					break
				}
			}
			v.Name = v.Letter
			if v.Name == "" || seen[v.Name] {
				v.Name = fmt.Sprintf("Partition %d", p.Number)
				if len(disks) > 1 {
					v.Name = fmt.Sprintf("Disk %d Partition %d", d.Number, p.Number)
				}
			}
			seen[v.Name] = true
			out = append(out, v)
		}
	}
	return out
}

func toAPIImageDisks(run Run) []apiImageDisk {
	disks := imageDetails(run)
	if len(disks) == 0 {
		return nil
	}
	folder := map[int]string{}
	for _, v := range imageVolumes(run) {
		folder[v.Ref] = v.Name
	}
	out := make([]apiImageDisk, 0, len(disks))
	for _, d := range disks {
		x := apiImageDisk{Index: d.Index, Number: d.Number, Model: d.Model, Size: d.Size, Style: d.Style, Partitions: []apiImagePartition{}}
		for _, p := range d.Partitions {
			x.Partitions = append(x.Partitions, apiImagePartition{Number: p.Number, Size: p.Length, FileSystem: p.FileSystem, Label: p.Label,
				MountPoints: p.MountPoints, Included: p.Included, Used: p.StoredBytes, Folder: folder[imaging.PartRef(d.Index, p.Number)]})
		}
		out = append(out, x)
	}
	return out
}

// imagePath splits an API path ("C/Users/ana") into the volume and the
// path inside it (`\Users\ana`).
func imagePath(run Run, p string) (imageVol, string, error) {
	comps := snapshotComps(p)
	if len(comps) == 0 {
		return imageVol{}, `\`, nil
	}
	for _, v := range imageVolumes(run) {
		if strings.EqualFold(v.Name, comps[0]) {
			return v, imaging.CleanPath(`\` + strings.Join(comps[1:], `\`)), nil
		}
	}
	return imageVol{}, "", notInBackupError(fmt.Sprintf("%s is not a volume of this backup", comps[0]))
}

// imageDisplayPath is the Windows path of an entry ("C:\Users\ana").
func imageDisplayPath(v imageVol, inner string) string {
	if v.Letter != "" {
		return v.Letter + ":" + inner
	}
	if inner == `\` {
		return v.Name
	}
	return v.Name + inner
}

func (s *Server) imageFileList(ctx context.Context, run Run, p string) (map[string]any, error) {
	v, inner, err := imagePath(run, p)
	if err != nil {
		return nil, apiErr(http.StatusNotFound, "%s", err.Error())
	}
	out := []apiFileEntry{}
	if v.Name == "" {
		for _, v := range imageVolumes(run) {
			out = append(out, apiFileEntry{Name: v.Name, Path: v.Name, Dir: true})
		}
		return map[string]any{"run_id": run.ID, "path": "", "windows": true, "display_path": "", "entries": out}, nil
	}
	vol, _, err := s.imageVolume(ctx, run, v.Ref)
	if err != nil {
		if errors.Is(err, ErrBrowseLocal) {
			err = errors.New("this backup is on a disk of an agent; the console cannot read it — restore it with POST /api/v1/runs/{id}/restore")
		}
		return nil, apiErr(http.StatusConflict, "%s", err.Error())
	}
	entries, err := vol.List(inner)
	if err != nil {
		return nil, apiErr(http.StatusNotFound, "%s", err.Error())
	}
	dir := v.Name + strings.ReplaceAll(strings.TrimRight(inner, `\`), `\`, "/")
	for _, e := range entries {
		out = append(out, apiFileEntry{Name: e.Name, Path: dir + "/" + e.Name, Dir: e.IsDir, Size: uint64(max(e.Size, 0)), Modified: e.ModTime})
	}
	return map[string]any{"run_id": run.ID, "path": dir, "windows": true, "display_path": imageDisplayPath(v, inner), "entries": out}, nil
}

// downloadImageFiles streams files of an image backup: one file as is,
// several or folders as a ZIP archive. Errors before the first byte are
// returned.
func (s *Server) downloadImageFiles(w http.ResponseWriter, r *http.Request, run Run, sel []string) error {
	ctx := r.Context()
	type item struct {
		v     imageVol
		inner string
		vol   *imaging.Volume
		e     imaging.Entry
	}
	var items []item
	for _, p := range sel {
		v, inner, err := imagePath(run, p)
		if err != nil {
			return err
		}
		if v.Name == "" {
			return notInBackupError("choose files or folders inside a volume")
		}
		vol, _, err := s.imageVolume(ctx, run, v.Ref)
		if err != nil {
			return err
		}
		e, err := vol.Stat(inner)
		if err != nil {
			return notInBackupError(fmt.Sprintf("%s is not in the backup", imageDisplayPath(v, inner)))
		}
		items = append(items, item{v, inner, vol, e})
	}
	s.audit(r, "restore.download", "%d paths from image backup #%d", len(sel), run.ID)
	if len(items) == 1 && !items[0].e.IsDir {
		rd, size, err := items[0].vol.ReadFile(items[0].inner)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(items[0].e.Name))
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		_, _ = io.Copy(w, rd)
		return nil
	}
	name := fmt.Sprintf("backup%d-%s.zip", run.ID, time.Now().Format("20060102-1504"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	zw := zip.NewWriter(w)
	defer zw.Close()
	var add func(vol *imaging.Volume, rel, inner string, e imaging.Entry) error
	add = func(vol *imaging.Volume, rel, inner string, e imaging.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.IsDir {
			rd, _, err := vol.ReadFile(inner)
			if err != nil {
				return err
			}
			zf, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: e.ModTime})
			if err != nil {
				return err
			}
			_, err = io.Copy(zf, rd)
			return err
		}
		list, err := vol.List(inner)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			_, _ = zw.CreateHeader(&zip.FileHeader{Name: rel + "/", Modified: e.ModTime})
		}
		for _, c := range list {
			if err := add(vol, path.Join(rel, c.Name), imaging.CleanPath(inner+`\`+c.Name), c); err != nil {
				return err
			}
		}
		return nil
	}
	for _, it := range items {
		rel := it.e.Name
		if it.inner == `\` {
			rel = it.v.Name
		}
		if add(it.vol, rel, it.inner, it.e) != nil {
			return nil // the archive is cut; the client sees a failed download
		}
	}
	return nil
}

// queueImageFileRestore restores files of an image backup: one run per
// volume. target "" restores to the original location.
func (s *Server) queueImageFileRestore(ctx context.Context, run Run, agentID int64, paths []string, target string) ([]int64, error) {
	if len(paths) == 0 {
		return nil, apiErr(http.StatusBadRequest, `give the files and folders in "paths" (to restore a whole disk use POST /api/v1/runs/{id}/restore-disk)`)
	}
	byVol := map[int][]string{}
	for _, p := range paths {
		v, inner, err := imagePath(run, p)
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "%s", err.Error())
		}
		if v.Name == "" {
			return nil, apiErr(http.StatusBadRequest, "choose files or folders inside a volume")
		}
		byVol[v.Ref] = append(byVol[v.Ref], inner)
	}
	refs := make([]int, 0, len(byVol))
	for ref := range byVol {
		refs = append(refs, ref)
	}
	sort.Ints(refs)
	var ids []int64
	for _, ref := range refs {
		id, err := s.store.QueueImageFileRestore(ctx, run.ID, agentID, ref, byVol[ref], target)
		if err != nil {
			return ids, apiErr(http.StatusConflict, "%s", err.Error())
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// apiDiskRestoreRequest is the body of POST /api/v1/runs/{id}/restore-disk.
type apiDiskRestoreRequest struct {
	AgentID int64 `json:"agent_id"` // the machine with the target disk; 0 = the backed-up one
	// ConfirmErase must be true: everything on the target disk is erased.
	ConfirmErase bool `json:"confirm_erase"`
	NewHardware  bool `json:"new_hardware"`

	// Image backups (Windows): the disk of the backup by its number when
	// it was backed up (Disk) or its position (Image), and the target disk.
	Disk        *int   `json:"disk"`
	Image       *int   `json:"image"`
	TargetDisk  *int   `json:"target_disk"`
	KeepOffline bool   `json:"keep_offline"`
	DriverPath  string `json:"driver_path"`

	// Linux system backups: the target device, e.g. /dev/sdb.
	Device string `json:"device"`
}

func (s *Server) registerAPIImage(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/runs/{id}/restore-disk", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		var req apiDiskRestoreRequest
		if err := decodeJSON(r, &req); err != nil {
			return nil, err
		}
		if !req.ConfirmErase {
			return nil, apiErr(http.StatusBadRequest, "set confirm_erase: everything on the target disk is erased")
		}
		b, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if b.Expired {
			return nil, apiErr(http.StatusConflict, "backup run %d was removed by the retention policy", id)
		}
		if req.AgentID == 0 {
			req.AgentID = b.AgentID
		}
		var rid int64
		switch b.Kind {
		case api.KindImageBackup:
			if req.TargetDisk == nil {
				return nil, apiErr(http.StatusBadRequest, "give target_disk, the number of the disk to overwrite")
			}
			img := 0
			switch {
			case req.Image != nil && req.Disk != nil:
				return nil, apiErr(http.StatusBadRequest, "give either disk or image, not both")
			case req.Image != nil:
				img = *req.Image
			case req.Disk != nil:
				img = -1
				for _, d := range imageDetails(b) {
					if d.Number == *req.Disk {
						img = d.Index
					}
				}
				if img < 0 {
					return nil, apiErr(http.StatusBadRequest, "disk %d is not in this backup", *req.Disk)
				}
			}
			rid, err = s.store.QueueImageRestore(r.Context(), id, req.AgentID, ImageRestoreOptions{TargetDisk: *req.TargetDisk, Image: img,
				KeepOffline: req.KeepOffline, NewHardware: req.NewHardware, DriverPath: req.DriverPath})
			if err == nil {
				s.audit(r, "restore.image", "run #%d from backup #%d (disk %d of the backup) to disk %d on agent %d", rid, id, img, *req.TargetDisk, req.AgentID)
			}
		case api.KindSystemBackup:
			if req.Device == "" {
				return nil, apiErr(http.StatusBadRequest, "give device, the disk to overwrite (e.g. /dev/sdb)")
			}
			o := api.SystemRestore{Mode: "disk", Device: req.Device, NewHardware: req.NewHardware}
			rid, err = s.store.QueueSystemRestore(r.Context(), id, req.AgentID, o)
			if err == nil {
				s.audit(r, "restore.system", "run #%d from backup #%d (disk %s) on agent %d", rid, id, req.Device, req.AgentID)
			}
		default:
			return nil, apiErr(http.StatusConflict, "backup run %d is not a disk image or Linux system backup", id)
		}
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, err
			}
			return nil, apiErr(http.StatusConflict, "%s", err.Error())
		}
		return map[string]int64{"run_id": rid}, nil
	}))
}
