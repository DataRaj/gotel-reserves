// Package summaries — openai.go
//
// Raw net/http Grok (xAI) client. No SDK dependency.
// The xAI API is OpenAI-compatible: same request envelope, same response
// shape, same Structured Outputs (json_schema / strict mode) contract.
//
// Endpoint: POST https://api.x.ai/v1/chat/completions
//
// Structured Outputs contract:
//   response_format.type = "json_schema"
//   response_format.json_schema = { strict: true, schema: <SummaryOutput schema> }
//
// Chunking (map-reduce):
//   Transcripts that exceed maxChunkRunes are split into overlapping windows.
//   Each chunk produces a partial SummaryOutput. A final reduce pass merges
//   all partials into a single SummaryOutput via a second LLM call so the
//   output is coherent, not a concatenated list.
//
// Token budget:
//   grok-3-mini context window = 131k tokens ≈ 524k chars (rough).
//   maxChunkRunes = 80_000 chars (safe margin with prompt overhead).
//   overlap = 500 chars to avoid cutting sentences mid-thought.
package summaries

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"recallo/internals/configs"
	"recallo/internals/logger"
)

const (
	grokChatURL = "https://api.x.ai/v1/chat/completions"

	// Chunking thresholds — tuned for grok-3-mini 131k context.
	maxChunkRunes = 80_000 // chars per chunk sent to the model
	chunkOverlap  = 500    // trailing chars carried into next chunk
)

// ── Output schema (Structured Outputs) ───────────────────────────────────────

// ActionItem is a single task extracted from the transcript.
type ActionItem struct {
	Assignee string `json:"assignee"` // participant name or "unassigned"
	Task     string `json:"task"`
	Deadline string `json:"deadline"` // ISO date or natural language, "" if none
}

// SummaryOutput is the canonical structured response from Grok.
// json tags drive both the JSON schema declaration and unmarshalling.
type SummaryOutput struct {
	ExecutiveSummary string       `json:"executive_summary"`
	KeyPoints        []string     `json:"key_points"`
	ActionItems      []ActionItem `json:"action_items"`
	DecisionsMade    []string     `json:"decisions_made"`
	DiscussionTags   []string     `json:"discussion_tags"`
}

// ── JSON schema declaration sent to Grok (Structured Outputs) ─────────────

// summaryOutputSchema is the JSON Schema object for SummaryOutput.
// Declared as a raw constant — cheaper than building it with reflection at runtime.
// schema must satisfy strict mode: no "additionalProperties", all fields required.
var summaryOutputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"executive_summary": map[string]any{"type": "string"},
		"key_points":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"action_items": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"assignee": map[string]any{"type": "string"},
					"task":     map[string]any{"type": "string"},
					"deadline": map[string]any{"type": "string"},
				},
				"required":             []string{"assignee", "task", "deadline"},
				"additionalProperties": false,
			},
		},
		"decisions_made":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"discussion_tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
	"required":             []string{"executive_summary", "key_points", "action_items", "decisions_made", "discussion_tags"},
	"additionalProperties": false,
}

// ── grokClient ────────────────────────────────────────────────────────────────

type grokClient struct {
	httpClient *http.Client
	cfg        configs.GrokConfig
}

func newGrokClient(cfg configs.GrokConfig) *grokClient {
	return &grokClient{
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSec) * time.Second,
		},
		cfg: cfg,
	}
}

// Summarize is the top-level entry point.
// If transcriptText fits within maxChunkRunes it calls summarizeChunk once.
// If it exceeds the threshold it runs the map-reduce pipeline.
func (c *grokClient) Summarize(ctx context.Context, systemPrompt, transcriptText string) (*SummaryOutput, error) {
	runes := []rune(transcriptText)
	if len(runes) <= maxChunkRunes {
		return c.summarizeChunk(ctx, systemPrompt, transcriptText)
	}
	return c.mapReduce(ctx, systemPrompt, runes)
}

// mapReduce splits the transcript into overlapping chunks, summarises each,
// then runs a reduce call that merges all partial outputs into one coherent result.
func (c *grokClient) mapReduce(ctx context.Context, systemPrompt string, runes []rune) (*SummaryOutput, error) {
	chunks := splitIntoChunks(runes, maxChunkRunes, chunkOverlap)
	logger.App.Printf("[summaries] map-reduce: %d chunks for %d runes", len(chunks), len(runes))

	partials := make([]SummaryOutput, 0, len(chunks))
	for i, chunk := range chunks {
		partial, err := c.summarizeChunk(ctx, systemPrompt,
			fmt.Sprintf("[Chunk %d/%d]\n%s", i+1, len(chunks), chunk))
		if err != nil {
			return nil, fmt.Errorf("grok.mapReduce chunk %d: %w", i, err)
		}
		partials = append(partials, *partial)
	}

	return c.reduce(ctx, partials)
}

// reduce merges partial SummaryOutputs into one final summary via a second LLM call.
func (c *grokClient) reduce(ctx context.Context, partials []SummaryOutput) (*SummaryOutput, error) {
	partialsJSON, err := json.MarshalIndent(partials, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("grok.reduce: marshal partials: %w", err)
	}

	reduceSystem := `You are a meeting summariser assistant performing a merge step.
You will receive an array of partial summaries (JSON) produced from consecutive chunks of a long transcript.
Merge them into ONE coherent, deduplicated SummaryOutput. Prefer concrete facts over duplicates.
Return valid JSON matching the schema exactly.`

	reduceUser := fmt.Sprintf("Partial summaries to merge:\n%s", partialsJSON)
	return c.chat(ctx, reduceSystem, reduceUser)
}

// summarizeChunk calls the Grok chat API for a single text chunk.
func (c *grokClient) summarizeChunk(ctx context.Context, systemPrompt, text string) (*SummaryOutput, error) {
	userMsg := fmt.Sprintf("Meeting transcript:\n\n%s", text)
	return c.chat(ctx, systemPrompt, userMsg)
}

// chat is the lowest-level call: builds the request, sends it, parses the response.
func (c *grokClient) chat(ctx context.Context, systemPrompt, userMsg string) (*SummaryOutput, error) {
	reqBody := map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userMsg},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "SummaryOutput",
				"strict": true,
				"schema": summaryOutputSchema,
			},
		},
		"temperature": 0.3, // low temp for factual extraction
		"max_tokens":  2048,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("grok.chat: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, grokChatURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("grok.chat: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grok.chat: http: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap
	if err != nil {
		return nil, fmt.Errorf("grok.chat: read body: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &GrokRateLimitError{RetryAfter: grokRetryAfter(resp)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grok.chat: status=%d body=%s", resp.StatusCode, truncate(rawBody, 512))
	}

	// Parse Grok chat completion envelope (OpenAI-compatible format).
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return nil, fmt.Errorf("grok.chat: unmarshal envelope: %w", err)
	}
	if len(envelope.Choices) == 0 {
		return nil, fmt.Errorf("grok.chat: no choices in response")
	}

	choice := envelope.Choices[0]
	if choice.FinishReason == "length" {
		return nil, fmt.Errorf("grok.chat: finish_reason=length — increase max_tokens or reduce chunk size")
	}

	var out SummaryOutput
	if err := json.Unmarshal([]byte(choice.Message.Content), &out); err != nil {
		return nil, fmt.Errorf("grok.chat: unmarshal SummaryOutput: content=%s err=%w",
			truncate([]byte(choice.Message.Content), 256), err)
	}

	return &out, nil
}

// ── Chunking ──────────────────────────────────────────────────────────────────

// splitIntoChunks slices runes into windows of maxSize with overlap trailing chars
// carried into the next window to avoid hard sentence cuts.
func splitIntoChunks(runes []rune, maxSize, overlap int) []string {
	var chunks []string
	step := maxSize - overlap
	if step <= 0 {
		step = maxSize
	}
	for start := 0; start < len(runes); start += step {
		end := start + maxSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[start:end]))
		if end == len(runes) {
			break
		}
	}
	return chunks
}

// ── Sentinel errors ───────────────────────────────────────────────────────────

// GrokRateLimitError signals a 429 from the Grok API.
// The worker pool's backoff absorbs it; the RetryAfter hint is logged.
type GrokRateLimitError struct {
	RetryAfter time.Duration
}

func (e *GrokRateLimitError) Error() string {
	return fmt.Sprintf("grok: rate limited, retry after %s", e.RetryAfter)
}

// IsRateLimit reports whether err is a Grok 429.
func IsRateLimit(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*GrokRateLimitError) //nolint:errorlint
	return ok
}

func grokRetryAfter(resp *http.Response) time.Duration {
	val := resp.Header.Get("Retry-After")
	if val == "" {
		return 60 * time.Second
	}
	d, err := time.ParseDuration(val + "s")
	if err != nil {
		return 60 * time.Second
	}
	return d
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func truncate(b []byte, max int) string {
	s := strings.ToValidUTF8(string(b), "")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
