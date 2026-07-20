package transcripts

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"recallo/internals/configs"
)

const (
	presignMinTTL = 15 * time.Minute
	presignMaxTTL = 4 * time.Hour
)

type spacesPresigner struct {
	cfg configs.SpacesConfig
}

func newSpacesPresigner(cfg configs.SpacesConfig) *spacesPresigner {
	return &spacesPresigner{cfg: cfg}
}

func (s *spacesPresigner) PresignGet(key string, durationHint time.Duration) (string, error) {
	if s.cfg.Endpoint == "" || s.cfg.Bucket == "" {
		return "", fmt.Errorf("spaces.PresignGet: endpoint and bucket must be configured")
	}

	ttl := presignTTL(durationHint)
	now := time.Now().UTC()
	expires := now.Add(ttl)

	// Fix 1: Ensure https:// scheme exists so Deepgram can download it
	endpoint := strings.TrimRight(s.cfg.Endpoint, "/")
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}

	// S3 path-style URL
	objectURL := fmt.Sprintf("%s/%s/%s", endpoint, s.cfg.Bucket, key)

	// String to sign for AWS SigV2
	stringToSign := fmt.Sprintf("GET\n\n\n%d\n/%s/%s", expires.Unix(), s.cfg.Bucket, key)

	sig := s.sign(stringToSign)

	u, err := url.Parse(objectURL)
	if err != nil {
		return "", fmt.Errorf("spaces.PresignGet: parse url: %w", err)
	}

	q := url.Values{}
	q.Set("AWSAccessKeyId", s.cfg.AccessKey)
	q.Set("Signature", sig)
	q.Set("Expires", fmt.Sprintf("%d", expires.Unix()))

	// Sort keys for deterministic output
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(q.Get(k)))
	}
	u.RawQuery = strings.Join(parts, "&")

	return u.String(), nil
}

func (s *spacesPresigner) sign(msg string) string {
	mac := hmac.New(sha1.New, []byte(s.cfg.SecretKey))
	mac.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// presignTTL computes the safe TTL given a recording duration.
// Strategy: 2× the recording duration, clamped to [15m, 4h].
func presignTTL(duration time.Duration) time.Duration {
	if duration <= 0 {
		return 60 * time.Minute
	}
	computed := duration * 2
	if computed < presignMinTTL {
		return presignMinTTL
	}
	if computed > presignMaxTTL {
		return presignMaxTTL
	}
	return computed
}
