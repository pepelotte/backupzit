package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// b2 talks to the Backblaze B2 native API (v2) with the provisioning key:
// it looks up the shared bucket and creates one application key per
// customer, restricted to the customer's folder.
type b2 struct {
	authURL       string // https://api.backblazeb2.com, replaced in tests
	keyID, appKey string
	http          *http.Client

	mu        sync.Mutex
	authAt    time.Time
	account   string
	apiURL    string
	authToken string
	s3URL     string
}

func newB2(keyID, appKey string) *b2 {
	return &b2{authURL: "https://api.backblazeb2.com", keyID: keyID, appKey: appKey, http: &http.Client{Timeout: time.Minute}}
}

// authorize gets an account token; it is valid for 24 hours, renewed after 12.
func (b *b2) authorize(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.authToken != "" && time.Since(b.authAt) < 12*time.Hour {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.authURL+"/b2api/v2/b2_authorize_account", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(b.keyID, b.appKey)
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Backblaze sign-in with the provisioning key failed: %w", readError(resp))
	}
	var a struct {
		AccountID          string `json:"accountId"`
		APIURL             string `json:"apiUrl"`
		AuthorizationToken string `json:"authorizationToken"`
		S3APIURL           string `json:"s3ApiUrl"`
	}
	if err := decodeJSONBody(resp, &a); err != nil {
		return err
	}
	b.account, b.apiURL, b.authToken, b.s3URL, b.authAt = a.AccountID, a.APIURL, a.AuthorizationToken, a.S3APIURL, time.Now()
	return nil
}

func (b *b2) call(ctx context.Context, op string, body, out any) error {
	if err := b.authorize(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	api, tok := b.apiURL, b.authToken
	b.mu.Unlock()
	err := doJSON(ctx, b.http, http.MethodPost, api+"/b2api/v2/"+op, map[string]string{"Authorization": tok}, body, out)
	var he *httpError
	if errors.As(err, &he) && he.Status == http.StatusUnauthorized {
		b.mu.Lock()
		b.authToken = "" // expired: sign in again next time
		b.mu.Unlock()
	}
	if err != nil {
		return fmt.Errorf("Backblaze %s: %w", op, err)
	}
	return nil
}

// bucket returns the id of the named bucket and checks that Object Lock is on.
func (b *b2) bucket(ctx context.Context, name string) (string, error) {
	var out struct {
		Buckets []struct {
			BucketID       string `json:"bucketId"`
			BucketName     string `json:"bucketName"`
			BucketType     string `json:"bucketType"`
			FileLockConfig struct {
				IsClientAuthorizedToRead bool `json:"isClientAuthorizedToRead"`
				Value                    struct {
					IsFileLockEnabled bool `json:"isFileLockEnabled"`
				} `json:"value"`
			} `json:"fileLockConfiguration"`
		} `json:"buckets"`
	}
	b.mu.Lock()
	acct := b.account
	b.mu.Unlock()
	if acct == "" {
		if err := b.authorize(ctx); err != nil {
			return "", err
		}
		b.mu.Lock()
		acct = b.account
		b.mu.Unlock()
	}
	if err := b.call(ctx, "b2_list_buckets", map[string]string{"accountId": acct, "bucketName": name}, &out); err != nil {
		return "", err
	}
	for _, x := range out.Buckets {
		if x.BucketName != name {
			continue
		}
		if x.BucketType != "allPrivate" {
			return "", fmt.Errorf("bucket %s is %s; it must be private", name, x.BucketType)
		}
		if x.FileLockConfig.IsClientAuthorizedToRead && !x.FileLockConfig.Value.IsFileLockEnabled {
			return "", fmt.Errorf("bucket %s has no Object Lock; create a new bucket with Object Lock enabled", name)
		}
		return x.BucketID, nil
	}
	return "", fmt.Errorf("bucket %s not found (or the provisioning key may not see it)", name)
}

// customerCapabilities are what one customer's key may do: its own files,
// Object Lock retention on them, and what S3 clients need to find the
// bucket. Not: change or delete buckets, create keys, bypass locks.
var customerCapabilities = []string{
	"listBuckets", "readBuckets", "readBucketRetentions",
	"listFiles", "readFiles", "writeFiles", "deleteFiles",
	"readFileRetentions", "writeFileRetentions",
}

type b2Key struct {
	ID, Secret string
}

func (b *b2) createKey(ctx context.Context, name, bucketID, prefix string) (b2Key, error) {
	b.mu.Lock()
	acct := b.account
	b.mu.Unlock()
	var out struct {
		ApplicationKeyID string `json:"applicationKeyId"`
		ApplicationKey   string `json:"applicationKey"`
	}
	err := b.call(ctx, "b2_create_key", map[string]any{
		"accountId": acct, "keyName": name, "capabilities": customerCapabilities,
		"bucketId": bucketID, "namePrefix": prefix,
	}, &out)
	if err == nil && (out.ApplicationKeyID == "" || out.ApplicationKey == "") {
		err = errors.New("Backblaze returned an empty key")
	}
	return b2Key{ID: out.ApplicationKeyID, Secret: out.ApplicationKey}, err
}

func (b *b2) deleteKey(ctx context.Context, id string) error {
	return b.call(ctx, "b2_delete_key", map[string]string{"applicationKeyId": id}, nil)
}

// s3Endpoint is the S3 host and region of the account, e.g.
// s3.eu-central-003.backblazeb2.com and eu-central-003.
func (b *b2) s3Endpoint() (host, region string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, err := url.Parse(b.s3URL)
	if err != nil || u.Host == "" {
		return "", ""
	}
	host = u.Host
	if m := regexp.MustCompile(`^s3\.([a-z0-9-]+)\.`).FindStringSubmatch(host); m != nil {
		region = m[1]
	}
	return host, region
}

// keyName is a valid B2 key name (letters, digits, hyphens; at most 100).
func keyName(s string) string {
	s = strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9-]+`).ReplaceAllString(s, "-"))
	if len(s) > 100 {
		s = s[:100]
	}
	return strings.Trim(s, "-")
}
