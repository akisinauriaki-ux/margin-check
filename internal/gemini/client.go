// Package gemini provides a minimal REST client for the Gemini generateContent API
// using Go's standard net/http library, with exact response caching, rate-limit pacing,
// and clean handling of per-minute vs per-day HTTP 429 limits.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is the official Gemini API host.
	DefaultBaseURL = "https://generativelanguage.googleapis.com"
	// DefaultMaxOutputTokens is at least 8192 so thinking models have budget for both thoughts and JSON output.
	DefaultMaxOutputTokens = 8192
	// DefaultTimeout is the per-request HTTP timeout.
	DefaultTimeout = 60 * time.Second
	// DefaultMinInterval paces live calls to stay within free-tier per-minute limits.
	DefaultMinInterval = 4 * time.Second
)

var (
	// ErrMissingAPIKey is returned when GEMINI_API_KEY is not set in the environment or .env.
	ErrMissingAPIKey = errors.New("gemini: missing GEMINI_API_KEY")
	// ErrDailyLimitReached is returned when a 429 response indicates a daily quota limit.
	ErrDailyLimitReached = errors.New("daily limit reached")
	// ErrToolsForbidden is returned if any caller attempts to include tools in a request.
	ErrToolsForbidden = errors.New("gemini: tools and function declarations are forbidden in the reader")
)

// Client is a small REST client for Gemini generateContent.
type Client struct {
	apiKey      string
	BaseURL     string
	HTTPClient  *http.Client
	Timeout     time.Duration
	MinInterval time.Duration
	CacheDir    string
	NoCache     bool

	mu       sync.Mutex
	lastCall time.Time
}

// NewClient creates a Gemini REST client. The apiKey is sent only in the x-goog-api-key header.
func NewClient(apiKey string, cacheDir string, noCache bool) (*Client, error) {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return nil, ErrMissingAPIKey
	}
	return &Client{
		apiKey:      key,
		BaseURL:     DefaultBaseURL,
		HTTPClient:  &http.Client{},
		Timeout:     DefaultTimeout,
		MinInterval: DefaultMinInterval,
		CacheDir:    cacheDir,
		NoCache:     noCache,
	}, nil
}

// LoadAPIKey reads GEMINI_API_KEY from the process environment or falls back to dotenvPath.
// The key value is never included in any error.
func LoadAPIKey(dotenvPath string) (string, error) {
	if v := strings.TrimSpace(os.Getenv("GEMINI_API_KEY")); v != "" {
		return v, nil
	}
	if strings.TrimSpace(dotenvPath) == "" {
		return "", ErrMissingAPIKey
	}
	f, err := os.Open(dotenvPath)
	if err != nil {
		return "", ErrMissingAPIKey
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		if k == "GEMINI_API_KEY" && v != "" {
			return v, nil
		}
	}
	return "", ErrMissingAPIKey
}

// Request describes a structured-output generateContent call with no tools.
type Request struct {
	Model              string
	InstructionVersion string
	SystemInstruction  string
	UserText           string
	ResponseSchema     map[string]any
	MaxOutputTokens    int
	ThinkingBudget     *int
}

// Response holds the parsed reply, token counts, model version, and cache status.
type Response struct {
	Text            string        `json:"text"`
	ModelVersion    string        `json:"model_version"`
	PromptTokens    int           `json:"prompt_tokens"`
	CandidateTokens int           `json:"candidate_tokens"`
	ThoughtTokens   int           `json:"thought_tokens"`
	TotalTokens     int           `json:"total_tokens"`
	Temperature     float64       `json:"temperature"`
	Source          string        `json:"source"` // "live" or "replayed"
	Replayed        bool          `json:"replayed"`
	CacheKey        string        `json:"cache_key"`
	RawResponse     string        `json:"raw_response"`
	Duration        time.Duration `json:"duration"`
}

type apiPart struct {
	Text    string `json:"text,omitempty"`
	Thought bool   `json:"thought,omitempty"`
}

type apiContent struct {
	Role  string    `json:"role,omitempty"`
	Parts []apiPart `json:"parts"`
}

type apiThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

type apiGenerationConfig struct {
	Temperature      float64            `json:"temperature"`
	MaxOutputTokens  int                `json:"maxOutputTokens"`
	ResponseMimeType string             `json:"responseMimeType,omitempty"`
	ResponseSchema   map[string]any     `json:"responseSchema,omitempty"`
	ThinkingConfig   *apiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type apiGenerateRequest struct {
	SystemInstruction *apiContent         `json:"systemInstruction,omitempty"`
	Contents          []apiContent        `json:"contents"`
	GenerationConfig  apiGenerationConfig `json:"generationConfig"`
}

type apiGenerateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []apiPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

type cacheEntry struct {
	Model              string `json:"model"`
	InstructionVersion string `json:"instruction_version"`
	CacheKey           string `json:"cache_key"`
	RawResponse        string `json:"raw_response"`
}

// Generate sends a structured-output generateContent request (or replays from exact cache).
func (c *Client) Generate(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	if strings.TrimSpace(req.Model) == "" {
		return Response{}, errors.New("gemini: model name is required")
	}
	maxOut := req.MaxOutputTokens
	if maxOut < DefaultMaxOutputTokens {
		maxOut = DefaultMaxOutputTokens
	}

	payload := apiGenerateRequest{
		Contents: []apiContent{
			{
				Role:  "user",
				Parts: []apiPart{{Text: req.UserText}},
			},
		},
		GenerationConfig: apiGenerationConfig{
			Temperature:      0,
			MaxOutputTokens:  maxOut,
			ResponseMimeType: "application/json",
			ResponseSchema:   req.ResponseSchema,
		},
	}
	if req.ThinkingBudget != nil {
		payload.GenerationConfig.ThinkingConfig = &apiThinkingConfig{
			ThinkingBudget: *req.ThinkingBudget,
		}
	}
	if strings.TrimSpace(req.SystemInstruction) != "" {
		payload.SystemInstruction = &apiContent{
			Parts: []apiPart{{Text: req.SystemInstruction}},
		}
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return Response{}, errors.New("gemini: failed to marshal request")
	}

	// Enforce capability separation: never allow tools or function declarations in the request body.
	var topKeys map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &topKeys); err == nil {
		if _, hasTools := topKeys["tools"]; hasTools {
			return Response{}, ErrToolsForbidden
		}
		if _, hasToolCfg := topKeys["toolConfig"]; hasToolCfg {
			return Response{}, ErrToolsForbidden
		}
	}

	cacheKey := computeCacheKey(req.Model, req.InstructionVersion, bodyBytes)

	// 1. Check exact reply cache unless NoCache is set.
	if !c.NoCache && strings.TrimSpace(c.CacheDir) != "" {
		if raw, ok := c.readCache(cacheKey); ok {
			resp, parseErr := parseAPIResponse(raw)
			if parseErr == nil {
				resp.Source = "replayed"
				resp.Replayed = true
				resp.CacheKey = cacheKey
				resp.Duration = time.Since(start)
				return resp, nil
			}
		}
	}

	// 2. Live call with pacing and at most 1 retry on a per-minute 429.
	rawResp, err := c.doWithSingleRateLimitRetry(ctx, req.Model, bodyBytes)
	if err != nil {
		return Response{}, c.scrubError(err)
	}

	resp, err := parseAPIResponse(rawResp)
	if err != nil {
		return Response{}, c.scrubError(err)
	}
	resp.Source = "live"
	resp.Replayed = false
	resp.CacheKey = cacheKey
	resp.Duration = time.Since(start)

	if strings.TrimSpace(c.CacheDir) != "" {
		_ = c.writeCache(cacheEntry{
			Model:              req.Model,
			InstructionVersion: req.InstructionVersion,
			CacheKey:           cacheKey,
			RawResponse:        rawResp,
		})
	}

	return resp, nil
}

func (c *Client) doWithSingleRateLimitRetry(ctx context.Context, model string, bodyBytes []byte) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.pace(ctx); err != nil {
			return "", err
		}
		status, headers, respBody, err := c.doOnce(ctx, model, bodyBytes)
		if err != nil {
			return "", err
		}
		if status >= 200 && status < 300 {
			return respBody, nil
		}
		if status == http.StatusTooManyRequests {
			if isDailyQuotaError(respBody) {
				return "", fmt.Errorf("%w: HTTP 429 daily quota exhausted for model %s", ErrDailyLimitReached, model)
			}
			// Per-minute limit: wait once as the reply says, then retry once. Never loop.
			if attempt == 0 {
				waitDur := parseRetryDelay(headers, respBody)
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(waitDur):
				}
				continue
			}
			return "", fmt.Errorf("gemini: HTTP 429 rate limit persisted after retry for model %s", model)
		}
		return "", fmt.Errorf("gemini: HTTP %d from model %s: %s", status, model, truncateSafe(c.scrubString(respBody), 300))
	}
	return "", errors.New("gemini: unexpected retry termination")
}

func (c *Client) pace(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.MinInterval <= 0 {
		c.lastCall = time.Now()
		return nil
	}
	if !c.lastCall.IsZero() {
		elapsed := time.Since(c.lastCall)
		if elapsed < c.MinInterval {
			wait := c.MinInterval - elapsed
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	c.lastCall = time.Now()
	return nil
}

func (c *Client) doOnce(ctx context.Context, model string, bodyBytes []byte) (int, http.Header, string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	modelPath := strings.TrimPrefix(strings.TrimSpace(model), "models/")
	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent", base, modelPath)

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return 0, nil, "", errors.New("gemini: failed to build HTTP request")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.apiKey)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return 0, nil, "", fmt.Errorf("gemini: request failed: %v", c.scrubError(err))
	}
	defer resp.Body.Close()

	rawBytes, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return resp.StatusCode, resp.Header, "", errors.New("gemini: failed reading response body")
	}
	return resp.StatusCode, resp.Header, string(rawBytes), nil
}

func parseAPIResponse(raw string) (Response, error) {
	var parsed apiGenerateResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return Response{}, fmt.Errorf("gemini: invalid JSON response: %w", err)
	}
	if len(parsed.Candidates) == 0 {
		return Response{}, errors.New("gemini: response contains no candidates")
	}
	var outText strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		if p.Thought {
			continue
		}
		outText.WriteString(p.Text)
	}
	text := strings.TrimSpace(outText.String())
	if text == "" {
		return Response{}, errors.New("gemini: candidate text is empty")
	}
	return Response{
		Text:            text,
		ModelVersion:    parsed.ModelVersion,
		PromptTokens:    parsed.UsageMetadata.PromptTokenCount,
		CandidateTokens: parsed.UsageMetadata.CandidatesTokenCount,
		ThoughtTokens:   parsed.UsageMetadata.ThoughtsTokenCount,
		TotalTokens:     parsed.UsageMetadata.TotalTokenCount,
		Temperature:     0,
		RawResponse:     raw,
	}, nil
}

func computeCacheKey(model, instructionVersion string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(strings.TrimSpace(model)))
	h.Write([]byte("\n"))
	h.Write([]byte(strings.TrimSpace(instructionVersion)))
	h.Write([]byte("\n"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Client) cacheFilePath(key string) string {
	return filepath.Join(c.CacheDir, key+".json")
}

func (c *Client) readCache(key string) (string, bool) {
	data, err := os.ReadFile(c.cacheFilePath(key))
	if err != nil {
		return "", false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil || strings.TrimSpace(entry.RawResponse) == "" {
		return "", false
	}
	return entry.RawResponse, true
}

func (c *Client) writeCache(entry cacheEntry) error {
	if err := os.MkdirAll(c.CacheDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := c.cacheFilePath(entry.CacheKey) + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, c.cacheFilePath(entry.CacheKey))
}

func isDailyQuotaError(body string) bool {
	lower := strings.ToLower(body)
	dailyMarkers := []string{
		"perday",
		"per_day",
		"per day",
		"requests per day",
		"tokens per day",
		"daily quota",
		"daily limit",
		"quota_exceeded",
		"generaterequestsperday",
		"rpd",
	}
	for _, m := range dailyMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

var (
	reRetryDelayJSON = regexp.MustCompile(`"retryDelay"\s*:\s*"([0-9]+(?:\.[0-9]+)?(?:ms|s))"`)
	reRetryInText    = regexp.MustCompile(`(?i)retry\s+in\s+([0-9]+(?:\.[0-9]+)?)\s*s`)
)

func parseRetryDelay(headers http.Header, body string) time.Duration {
	if headers != nil {
		if ra := strings.TrimSpace(headers.Get("Retry-After")); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
				return time.Duration(secs) * time.Second
			}
			if d, err := time.ParseDuration(ra); err == nil && d >= 0 {
				return d
			}
		}
	}
	if m := reRetryDelayJSON.FindStringSubmatch(body); m != nil {
		if d, err := time.ParseDuration(m[1]); err == nil && d >= 0 {
			return d
		}
	}
	if m := reRetryInText.FindStringSubmatch(body); m != nil {
		if d, err := time.ParseDuration(m[1] + "s"); err == nil && d >= 0 {
			return d
		}
	}
	return 2 * time.Second
}

func (c *Client) scrubString(s string) string {
	if c == nil || c.apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.apiKey, "[REDACTED]")
}

func (c *Client) scrubError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrDailyLimitReached) {
		return fmt.Errorf("%w: %s", ErrDailyLimitReached, c.scrubString(err.Error()))
	}
	return errors.New(c.scrubString(err.Error()))
}

func truncateSafe(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
