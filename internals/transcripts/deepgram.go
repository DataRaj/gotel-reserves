// Package transcripts — deepgram.go
//
// Deepgram pre-recorded (batch) REST API client.
//
// API contract used:
//
//	POST https://api.deepgram.com/v1/listen
//	Body: {"url": "<presigned-spaces-url>"}
//	Query params: model, language, smart_format, punctuate, diarize,
//	              utterances, words (word-level timestamps)
//
// The client never downloads the file itself. It hands Deepgram a
// short-lived presigned GET URL; Deepgram fetches and processes the media
// directly. This keeps bandwidth and memory pressure off the Go server.
//
// Presigned URL TTL must be longer than the expected Deepgram processing
// time. Rule: ceil(duration_sec / 60) * 2 minutes, min 15 min, max 4 hrs.
package transcripts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"recallo/internals/configs"
	"recallo/internals/logger"
)

const deepgramListenURL = "https://api.deepgram.com/v1/listen"

// ── Deepgram response structures ─────────────────────────────────────────────
// Only the fields we actually persist. Deepgram returns more; we discard the rest.

// DeepgramResponse is the top-level response envelope from /v1/listen.
type DeepgramResponse struct {
	Metadata DeepgramMetadata `json:"metadata"`
	Results  DeepgramResults  `json:"results"`
}

type DeepgramMetadata struct {
	RequestID string  `json:"request_id"`
	Duration  float64 `json:"duration"` // total audio duration in seconds
	ModelInfo struct {
		Name string `json:"name"`
	} `json:"model_info"`
}

type DeepgramResults struct {
	Channels []DeepgramChannel `json:"channels"`
}

type DeepgramChannel struct {
	Alternatives []DeepgramAlternative `json:"alternatives"`
}

// DeepgramAlternative holds the transcript text, confidence, and word timestamps.
type DeepgramAlternative struct {
	Transcript string       `json:"transcript"`
	Confidence float64      `json:"confidence"`
	Words      []WordResult `json:"words"`
}

// WordResult is stored as JSONB in transcripts.words_json.
// Used by the summarisation pipeline for accurate chunking by speaker/time.
type WordResult struct {
	Word        string  `json:"word"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Confidence  float64 `json:"confidence"`
	Speaker     int     `json:"speaker,omitempty"` // diarization speaker index
	SpeakerConf float64 `json:"speaker_confidence,omitempty"`
}

// ParsedTranscript is the distilled output handed back to the service layer.
type ParsedTranscript struct {
	Text        string
	Words       []WordResult
	Confidence  float64
	DurationSec int
	Model       string
	Language    string
}

// ── deepgramClient ────────────────────────────────────────────────────────────

type deepgramClient struct {
	httpClient *http.Client
	cfg        configs.DeepgramConfig
}

func newDeepgramClient(cfg configs.DeepgramConfig) *deepgramClient {
	return &deepgramClient{
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSec) * time.Second,
		},
		cfg: cfg,
	}
}

// Transcribe submits a presigned media URL to Deepgram's batch API and
// returns the parsed transcript. The caller is responsible for presigning.
//
// Context cancellation aborts the in-flight HTTP request cleanly.
func (c *deepgramClient) Transcribe(ctx context.Context, presignedURL string) (*ParsedTranscript, error) {
	body, err := json.Marshal(map[string]string{"url": presignedURL})
	if err != nil {
		return nil, fmt.Errorf("deepgram.Transcribe: marshal body: %w", err)
	}

	apiURL := c.buildURL()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("deepgram.Transcribe: build request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deepgram.Transcribe: http: %w", err)
	}
	defer resp.Body.Close()

	// Read the full body before checking status — Deepgram puts error detail in the body.
	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MB cap
	if err != nil {
		return nil, fmt.Errorf("deepgram.Transcribe: read body: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{RetryAfter: retryAfterHeader(resp)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deepgram.Transcribe: status=%d body=%s", resp.StatusCode, truncate(rawBody, 512))
	}

	var dg DeepgramResponse
	if err := json.Unmarshal(rawBody, &dg); err != nil {
		return nil, fmt.Errorf("deepgram.Transcribe: unmarshal response: %w", err)
	}

	return c.parse(&dg), nil
}

// buildURL assembles the /v1/listen endpoint with query params.
func (c *deepgramClient) buildURL() string {
	u, _ := url.Parse(deepgramListenURL)
	q := u.Query()
	q.Set("model", c.cfg.Model)
	q.Set("language", c.cfg.Language)
	q.Set("smart_format", "true") // auto-applies punctuation, numerals, paragraphs
	q.Set("punctuate", "true")
	q.Set("diarize", "true")    // speaker labels on word-level timestamps
	q.Set("utterances", "true") // utterance segmentation
	q.Set("words", "true")      // word-level timestamps → stored in words_json
	u.RawQuery = q.Encode()
	return u.String()
}

// parse extracts the first channel's best alternative.
// Deepgram always returns at least one channel and one alternative for valid audio.
func (c *deepgramClient) parse(dg *DeepgramResponse) *ParsedTranscript {
	if len(dg.Results.Channels) == 0 || len(dg.Results.Channels[0].Alternatives) == 0 {
		logger.App.Printf("[deepgram] empty channels/alternatives in response req_id=%s", dg.Metadata.RequestID)
		return &ParsedTranscript{Model: c.cfg.Model, Language: c.cfg.Language}
	}
	alt := dg.Results.Channels[0].Alternatives[0]
	return &ParsedTranscript{
		Text:        alt.Transcript,
		Words:       alt.Words,
		Confidence:  alt.Confidence,
		DurationSec: int(dg.Metadata.Duration),
		Model:       dg.Metadata.ModelInfo.Name,
		Language:    c.cfg.Language,
	}
}

// ── Sentinel errors ───────────────────────────────────────────────────────────

// RateLimitError signals Deepgram returned 429. The worker applies additional
// backoff on top of the standard exponential schedule when this is returned.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("deepgram: rate limited, retry after %s", e.RetryAfter)
}

// IsRateLimit reports whether err is a Deepgram 429 error.
func IsRateLimit(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*RateLimitError) //nolint:errorlint
	return ok
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func retryAfterHeader(resp *http.Response) time.Duration {
	val := resp.Header.Get("Retry-After")
	if val == "" {
		return 60 * time.Second // conservative default
	}
	d, err := time.ParseDuration(val + "s")
	if err != nil {
		return 60 * time.Second
	}
	return d
}

func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}
