package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is /etc/backupzit-connector/config.json (mode 0600, owned by the
// connector's user). Secrets can also come from the environment, which
// overrides the file: BZC_PLATFORM_TOKEN, BZC_BACKUPZIT_TOKEN,
// BZC_B2_KEY_ID, BZC_B2_APP_KEY.
type Config struct {
	Platform struct {
		// URL of the platform's connector endpoint, e.g.
		// https://<project>.supabase.co/functions/v1/backupzit-connector
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"platform"`
	BackupZit struct {
		URL         string `json:"url"`         // https://127.0.0.1:8443
		Token       string `json:"token"`       // API token of the user svc-connector
		Fingerprint string `json:"fingerprint"` // SHA256:… of the console certificate
	} `json:"backupzit"`
	B2 struct {
		// The provisioning key: may list buckets and create and delete
		// application keys. It never leaves this server.
		KeyID  string `json:"key_id"`
		AppKey string `json:"app_key"`
		// The shared bucket for all customers' backups (Object Lock on).
		Bucket string `json:"bucket"`
		// Days every backup file stays immutable (Object Lock).
		LockDays int `json:"lock_days"`
	} `json:"b2"`
	PollSeconds   int `json:"poll_seconds"`   // default 5
	StatusSeconds int `json:"status_seconds"` // default 60
	RunDays       int `json:"run_days"`       // runs reported, default 90
	// TempDir holds downloads on their way to the platform.
	TempDir string `json:"temp_dir"`
}

func loadConfig(path string) (*Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s holds secrets and must not be readable by others (chmod 600)", path)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	env := func(dst *string, name string) {
		if v := os.Getenv(name); v != "" {
			*dst = v
		}
	}
	env(&c.Platform.Token, "BZC_PLATFORM_TOKEN")
	env(&c.BackupZit.Token, "BZC_BACKUPZIT_TOKEN")
	env(&c.B2.KeyID, "BZC_B2_KEY_ID")
	env(&c.B2.AppKey, "BZC_B2_APP_KEY")
	if c.BackupZit.URL == "" {
		c.BackupZit.URL = "https://127.0.0.1:8443"
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = 5
	}
	if c.StatusSeconds <= 0 {
		c.StatusSeconds = 60
	}
	if c.RunDays <= 0 {
		c.RunDays = 90
	}
	if c.B2.LockDays <= 0 {
		c.B2.LockDays = 30
	}
	if c.TempDir == "" {
		c.TempDir = os.TempDir()
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	var miss []string
	for name, v := range map[string]string{
		"platform.url": c.Platform.URL, "platform.token": c.Platform.Token,
		"backupzit.token": c.BackupZit.Token,
	} {
		if strings.TrimSpace(v) == "" {
			miss = append(miss, name)
		}
	}
	if len(miss) > 0 {
		return fmt.Errorf("missing in the configuration: %s", strings.Join(miss, ", "))
	}
	// The example configuration has explanations where secrets belong.
	for name, v := range map[string]string{"platform.token": c.Platform.Token, "backupzit.token": c.BackupZit.Token,
		"backupzit.fingerprint": c.BackupZit.Fingerprint, "b2.key_id": c.B2.KeyID, "b2.app_key": c.B2.AppKey} {
		if strings.ContainsAny(v, " …") {
			return fmt.Errorf("%s is not filled in yet (edit /etc/backupzit-connector/config.json)", name)
		}
	}
	if !strings.HasPrefix(c.Platform.URL, "https://") {
		return errors.New("platform.url must be https://")
	}
	if strings.HasPrefix(c.BackupZit.URL, "https://") && c.BackupZit.Fingerprint == "" {
		return errors.New("backupzit.fingerprint is required (journalctl -u backupzit-server | grep fingerprint)")
	}
	if c.B2.LockDays > 3650 {
		return errors.New("b2.lock_days must be at most 3650")
	}
	return nil
}

// b2Ready tells whether customer storage can be provisioned.
func (c *Config) b2Ready() bool {
	return c.B2.KeyID != "" && c.B2.AppKey != "" && c.B2.Bucket != ""
}

func (c *Config) pollEvery() time.Duration   { return time.Duration(c.PollSeconds) * time.Second }
func (c *Config) statusEvery() time.Duration { return time.Duration(c.StatusSeconds) * time.Second }
