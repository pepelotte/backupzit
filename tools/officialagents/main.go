// Command officialagents downloads the agent and hardened repository
// packages of an official BackupZit release, checked against the signed
// release manifest, so a console built from this fork ships the official,
// signed agents of the same version:
//
//	go run ./tools/officialagents 0.48.2 bin/linux/agents
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
	"time"

	"github.com/max-zit/backupzit/internal/update"
)

const releases = "https://github.com/Max-Zit/backupzit/releases/download/v"

var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func main() {
	if len(os.Args) != 3 || !versionRe.MatchString(os.Args[1]) {
		fmt.Fprintln(os.Stderr, "usage: officialagents <version, e.g. 0.48.2> <output folder>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "officialagents:", err)
		os.Exit(1)
	}
}

func run(version, out string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	base := releases + version + "/"
	raw, err := get(ctx, base+"manifest.json", 1<<20)
	if err != nil {
		return err
	}
	sig, err := get(ctx, base+"manifest.json.sig", 4096)
	if err != nil {
		return err
	}
	m, err := update.Verify(raw, string(sig), nil)
	if err != nil {
		return err
	}
	if m.Version != version {
		return fmt.Errorf("manifest is for version %s, not %s", m.Version, version)
	}
	if err := os.MkdirAll(out, 0o755); err != nil { // #nosec G301 -- packages for download from the console
		return err
	}
	n := 0
	for _, f := range m.Files {
		if f.Kind != "agent" && f.Kind != "repo" {
			continue
		}
		b, err := get(ctx, base+f.Name, 1<<30)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(b)) != f.Size {
			return fmt.Errorf("%s does not match the signed manifest", f.Name)
		}
		dst := filepath.Join(out, filepath.Base(f.Name))
		if err := os.WriteFile(dst, b, 0o644); err != nil { // #nosec G306 -- public packages, served by the console
			return err
		}
		fmt.Printf("%s  %s\n", f.SHA256, f.Name)
		n++
	}
	if n == 0 {
		return fmt.Errorf("release %s lists no agent packages", version)
	}
	return nil
}

func get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G107 -- fixed GitHub release address
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: too large", url)
	}
	return b, nil
}
