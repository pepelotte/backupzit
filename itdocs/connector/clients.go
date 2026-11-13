package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// httpError is a non-2xx answer; Msg is the "error" field when there is one.
type httpError struct {
	Status int
	Msg    string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg) }

func readError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var x struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &x) == nil {
		if x.Error != "" {
			msg = x.Error
		} else if x.Message != "" {
			msg = x.Message
		}
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return &httpError{Status: resp.StatusCode, Msg: msg}
}

// doJSON sends body as JSON and decodes a JSON answer into out (if not nil).
func doJSON(ctx context.Context, c *http.Client, method, url string, header map[string]string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(out)
}

func decodeJSONBody(resp *http.Response, out any) error {
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// ---- BackupZit console (local REST API)

type backupZit struct {
	url, token string
	http       *http.Client
}

// newBackupZit pins the console's self-signed certificate by its
// fingerprint ("SHA256:<base64>", as the console logs it).
func newBackupZit(url, token, fingerprint string) *backupZit {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // always local
	if fingerprint != "" {
		want := strings.TrimSpace(fingerprint)
		tr.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, // replaced by the pin below
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("console sent no certificate")
				}
				if got := certFingerprint(cs.PeerCertificates[0]); got != want {
					return fmt.Errorf("console certificate %s does not match the configured %s", got, want)
				}
				return nil
			},
		}
	}
	return &backupZit{url: strings.TrimRight(url, "/"), token: token, http: &http.Client{Transport: tr, Timeout: 10 * time.Minute}}
}

func certFingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}

func (b *backupZit) call(ctx context.Context, method, path string, body, out any) error {
	return doJSON(ctx, b.http, method, b.url+"/api/v1"+path, map[string]string{"Authorization": "Bearer " + b.token}, body, out)
}

// download streams POST /runs/{id}/files/download into a temporary file.
func (b *backupZit) download(ctx context.Context, runID int64, paths []string, dir string) (file, name string, size int64, err error) {
	body, _ := json.Marshal(map[string]any{"paths": paths})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/runs/%d/files/download", b.url, runID), bytes.NewReader(body))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, readError(resp)
	}
	name = dispositionName(resp.Header.Get("Content-Disposition"))
	if name == "" {
		name = fmt.Sprintf("backup-%d.zip", runID)
	}
	f, err := os.CreateTemp(dir, "bzc-download-*")
	if err != nil {
		return "", "", 0, err
	}
	size, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", "", 0, err
	}
	return f.Name(), name, size, nil
}

// dispositionName reads the file name of a Content-Disposition header and
// keeps it a plain name.
func dispositionName(h string) string {
	_, after, ok := strings.Cut(h, "filename=")
	if !ok {
		return ""
	}
	n := strings.Trim(strings.TrimSpace(strings.Split(after, ";")[0]), `"`)
	n = strings.NewReplacer("/", "_", `\`, "_", "..", "_").Replace(n)
	if len(n) > 200 {
		n = n[:200]
	}
	return n
}

// ---- itdocs platform (Supabase edge function)

type platform struct {
	url, token string
	http       *http.Client
}

func newPlatform(url, token string) *platform {
	return &platform{url: strings.TrimRight(url, "/"), token: token, http: &http.Client{Timeout: 2 * time.Minute}}
}

func (p *platform) call(ctx context.Context, path string, body, out any) error {
	return doJSON(ctx, p.http, http.MethodPost, p.url+path, map[string]string{"Authorization": "Bearer " + p.token}, body, out)
}

// Command is a job for the connector, as /commands/claim returns it.
type Command struct {
	ID         string          `json:"id"`
	CustomerID string          `json:"customer_id"`
	DeviceID   string          `json:"device_id,omitempty"`
	Action     string          `json:"action"`
	Params     json.RawMessage `json:"params"`
}

func (p *platform) claim(ctx context.Context, limit int) ([]Command, error) {
	var out struct {
		Commands []Command `json:"commands"`
	}
	err := p.call(ctx, "/commands/claim", map[string]int{"limit": limit}, &out)
	return out.Commands, err
}

func (p *platform) complete(ctx context.Context, id string, result any, failure error) error {
	body := map[string]any{"status": "done", "result": result}
	if failure != nil {
		body = map[string]any{"status": "failed", "result": result, "error": failure.Error()}
	}
	if body["result"] == nil {
		body["result"] = map[string]any{}
	}
	return p.call(ctx, "/commands/"+id+"/complete", body, nil)
}

func (p *platform) status(ctx context.Context, s any) error { return p.call(ctx, "/status", s, nil) }

// upload asks for a signed upload URL for a command and PUTs the file;
// extra goes into the request (e.g. "platform" for agent installers).
func (p *platform) upload(ctx context.Context, commandID, name, file string, extra ...string) (string, error) {
	var u struct {
		StoragePath string `json:"storage_path"`
		SignedURL   string `json:"signed_url"`
	}
	body := map[string]string{"command_id": commandID, "filename": name}
	for i := 0; i+1 < len(extra); i += 2 {
		body[extra[i]] = extra[i+1]
	}
	if err := p.call(ctx, "/downloads/upload-url", body, &u); err != nil {
		return "", err
	}
	if u.SignedURL == "" {
		return "", errors.New("platform returned no upload URL")
	}
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.SignedURL, f)
	if err != nil {
		return "", err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", contentType(name))
	resp, err := (&http.Client{Timeout: 2 * time.Hour}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", readError(resp)
	}
	return u.StoragePath, nil
}

func contentType(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".zip") {
		return "application/zip"
	}
	return "application/octet-stream"
}
