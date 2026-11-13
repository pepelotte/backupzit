package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// agent.installer: after a technician approved the installation on a
// device, the platform asks for the installer of the BackupZit agent. The
// connector creates a single-use enrollment code that carries the
// customer and device, and hands over the official agent package the
// console ships, with what the platform's management agent needs to run
// it silently.

// installerCodeMinutes is how long the code of an installer stays valid.
// The management agent installs within minutes; the agent installer keeps
// trying to enroll when the console is not reachable at that moment.
const installerCodeMinutes = 24 * 60

var rpmOS = regexp.MustCompile(`(?i)rhel|red ?hat|centos|alma|rocky|fedora|suse|oracle`)

type agentPackage struct {
	Platform    string   `json:"platform"`
	PackageType string   `json:"package_type"` // msi, deb or rpm
	StoragePath string   `json:"storage_path"`
	FileName    string   `json:"file_name"`
	SHA256      string   `json:"sha256"`
	SizeBytes   int64    `json:"size_bytes"`
	Version     string   `json:"version"`
	InstallArgs []string `json:"install_args"`
	// PostInstall: commands to run after installing a Linux package.
	PostInstall [][]string `json:"post_install,omitempty"`
}

var pkgVersion = regexp.MustCompile(`backupzit-agent[-_](\d+\.\d+\.\d+)`)

// findPackage picks the official agent package for a platform.
func findPackage(dist, platform, osName string) (file, kind string, err error) {
	var pattern string
	switch platform {
	case "windows-x64":
		pattern, kind = "backupzit-agent-*-x64.msi", "msi"
	case "linux-x64":
		pattern, kind = "backupzit-agent_*_amd64.deb", "deb"
		if rpmOS.MatchString(osName) {
			pattern, kind = "backupzit-agent-*.x86_64.rpm", "rpm"
		}
	default:
		return "", "", fail("Plattform %q wird nicht unterstützt (windows-x64, linux-x64)", platform)
	}
	m, _ := filepath.Glob(filepath.Join(dist, pattern))
	if len(m) == 0 {
		return "", "", fail("kein BackupZit-Agentenpaket für %s auf dem Backup-Server gefunden (%s)", platform, dist)
	}
	sort.Strings(m)
	return m[len(m)-1], kind, nil
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func actAgentInstaller(ctx context.Context, c *connector, cmd Command) (any, error) {
	p, err := params[struct {
		RequestID string   `json:"request_id"`
		Hostname  string   `json:"hostname"`
		OS        string   `json:"os"`
		Platforms []string `json:"platforms"`
	}](cmd)
	if err != nil {
		return nil, err
	}
	if len(p.Platforms) == 0 {
		return nil, fail("keine Plattform angegeben")
	}
	// Check the packages before creating a code.
	type found struct{ platform, file, kind string }
	var pkgs []found
	for _, pl := range p.Platforms {
		file, kind, err := findPackage(c.cfg.DistDir, pl, p.OS)
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, found{pl, file, kind})
	}
	label := "itdocs:" + cmd.CustomerID + ":" + cmd.DeviceID
	if cmd.DeviceID == "" {
		label = "itdocs:" + cmd.CustomerID + ":request:" + p.RequestID
	}
	var code struct {
		Code    string `json:"code"`
		Expires string `json:"expires"`
	}
	if err := c.bz.call(ctx, http.MethodPost, "/agents/enroll-code", map[string]any{
		"ttl_minutes": installerCodeMinutes, "single_use": true, "label": label,
	}, &code); err != nil {
		return nil, err
	}
	var out []agentPackage
	for _, f := range pkgs {
		sum, size, err := fileSHA256(f.file)
		if err != nil {
			return nil, err
		}
		name := filepath.Base(f.file)
		path, err := c.pf.upload(ctx, cmd.ID, name, f.file, "platform", f.platform)
		if err != nil {
			return nil, fmt.Errorf("Hochladen zur Plattform: %w", err)
		}
		ap := agentPackage{Platform: f.platform, PackageType: f.kind, StoragePath: path, FileName: name, SHA256: sum, SizeBytes: size}
		if m := pkgVersion.FindStringSubmatch(name); m != nil {
			ap.Version = m[1]
		}
		if f.kind == "msi" {
			// msiexec /i <file> /qn /norestart ENROLLCODE=…
			ap.InstallArgs = []string{"/qn", "/norestart", "ENROLLCODE=" + code.Code}
		} else {
			// The package manager installs the file; then the agent enrolls.
			ap.InstallArgs = []string{}
			ap.PostInstall = [][]string{
				{"backupzit-agent", "enroll", "--if-not-enrolled", "--pending-ok", "--code", code.Code},
				{"systemctl", "restart", "backupzit-agent"},
			}
		}
		out = append(out, ap)
	}
	c.log.Info("agent installer prepared", "customer", cmd.CustomerID, "device", cmd.DeviceID, "host", p.Hostname,
		"platforms", strings.Join(p.Platforms, ","), "code_expires", code.Expires)
	return map[string]any{"packages": out, "enroll_label": label, "code_expires": code.Expires, "single_use": true}, nil
}
