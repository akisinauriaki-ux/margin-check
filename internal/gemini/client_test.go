package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGeminiClientFakeServer(t *testing.T) {
	const fakeKey = "AIzaSyFAKE_TEST_SECRET_KEY_987654321"

	t.Run("key in x-goog-api-key header not URL and no tools in body", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			if strings.Contains(r.URL.String(), fakeKey) || strings.Contains(r.URL.RawQuery, "key=") {
				t.Errorf("API key or key= query param found in URL: %s", r.URL.String())
			}
			if got := r.Header.Get("x-goog-api-key"); got != fakeKey {
				t.Errorf("x-goog-api-key header = %q, want %q", got, fakeKey)
			}
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			bodyStr := string(bodyBytes)
			if strings.Contains(bodyStr, `"tools"`) || strings.Contains(bodyStr, `"functionDeclarations"`) {
				t.Errorf("request body must never contain tools: %s", bodyStr)
			}
			var parsed map[string]any
			if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
				t.Fatalf("invalid request JSON: %v", err)
			}
			if _, ok := parsed["tools"]; ok {
				t.Errorf("request JSON has tools key")
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"candidates": [{"content": {"parts": [{"text": "{\"ok\":true}"}]}}],
				"usageMetadata": {"promptTokenCount": 42, "candidatesTokenCount": 8, "thoughtsTokenCount": 10, "totalTokenCount": 60},
				"modelVersion": "gemini-3.5-flash-lite-001"
			}`))
		}))
		defer srv.Close()

		cacheDir := t.TempDir()
		cli, err := NewClient(fakeKey, cacheDir, false)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		cli.BaseURL = srv.URL
		cli.MinInterval = 0

		req := Request{
			Model:              "gemini-3.5-flash-lite",
			InstructionVersion: "reader-v1",
			SystemInstruction:  "Extract deal JSON.",
			UserText:           "40 boxes at $40.",
			ResponseSchema:     map[string]any{"type": "object"},
		}
		resp, err := cli.Generate(context.Background(), req)
		if err != nil {
			t.Fatalf("Generate live call failed: %v", err)
		}
		if resp.Source != "live" || resp.Replayed {
			t.Errorf("first call Source=%q Replayed=%v, want live/false", resp.Source, resp.Replayed)
		}
		if resp.ModelVersion != "gemini-3.5-flash-lite-001" || resp.TotalTokens != 60 {
			t.Errorf("unexpected metadata: version=%q tokens=%d", resp.ModelVersion, resp.TotalTokens)
		}
		if atomic.LoadInt32(&calls) != 1 {
			t.Fatalf("expected 1 network call, got %d", atomic.LoadInt32(&calls))
		}

		// Second identical call must hit cache and make 0 additional network calls.
		resp2, err := cli.Generate(context.Background(), req)
		if err != nil {
			t.Fatalf("Generate cached call failed: %v", err)
		}
		if resp2.Source != "replayed" || !resp2.Replayed {
			t.Errorf("cached call Source=%q Replayed=%v, want replayed/true", resp2.Source, resp2.Replayed)
		}
		if atomic.LoadInt32(&calls) != 1 {
			t.Errorf("cache hit made network call: total calls = %d, want 1", atomic.LoadInt32(&calls))
		}
	})

	t.Run("daily 429 stops immediately without retry", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{
				"error": {
					"code": "quota_exceeded",
					"status": "RESOURCE_EXHAUSTED",
					"message": "Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 20 PerDayPerProject"
				}
			}`))
		}))
		defer srv.Close()

		cli, err := NewClient(fakeKey, t.TempDir(), true)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		cli.BaseURL = srv.URL
		cli.MinInterval = 0

		_, err = cli.Generate(context.Background(), Request{
			Model:              "gemini-3.5-flash-lite",
			InstructionVersion: "reader-v1",
			UserText:           "test",
		})
		if err == nil {
			t.Fatal("expected error on daily 429, got nil")
		}
		if !errors.Is(err, ErrDailyLimitReached) || !strings.Contains(err.Error(), "daily limit reached") {
			t.Errorf("error = %v, want ErrDailyLimitReached containing 'daily limit reached'", err)
		}
		if strings.Contains(err.Error(), fakeKey) {
			t.Errorf("error leaked API key: %v", err)
		}
		if atomic.LoadInt32(&calls) != 1 {
			t.Errorf("daily 429 must not retry: calls = %d, want 1", atomic.LoadInt32(&calls))
		}
	})

	t.Run("per-minute 429 waits once and retries once", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{
					"error": {
						"code": "rate_limit_exceeded",
						"status": "RESOURCE_EXHAUSTED",
						"message": "Rate limit per minute exceeded. Please retry in 0.01s.",
						"details": [{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "10ms"}]
					}
				}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"candidates": [{"content": {"parts": [{"text": "{\"ok\":true}"}]}}],
				"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15},
				"modelVersion": "gemini-3.5-flash-lite-001"
			}`))
		}))
		defer srv.Close()

		cli, err := NewClient(fakeKey, t.TempDir(), true)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		cli.BaseURL = srv.URL
		cli.MinInterval = 0

		resp, err := cli.Generate(context.Background(), Request{
			Model:              "gemini-3.5-flash-lite",
			InstructionVersion: "reader-v1",
			UserText:           "test",
		})
		if err != nil {
			t.Fatalf("expected retry on per-minute 429 to succeed, got: %v", err)
		}
		if resp.TotalTokens != 15 {
			t.Errorf("TotalTokens = %d, want 15", resp.TotalTokens)
		}
		if atomic.LoadInt32(&calls) != 2 {
			t.Errorf("calls = %d, want 2 (initial + 1 retry)", atomic.LoadInt32(&calls))
		}
	})

	t.Run("key never appears in error text even if echoed by server", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("bad request for key " + fakeKey))
		}))
		defer srv.Close()

		cli, err := NewClient(fakeKey, t.TempDir(), true)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		cli.BaseURL = srv.URL
		cli.MinInterval = 0
		cli.Timeout = 5 * time.Second

		_, err = cli.Generate(context.Background(), Request{
			Model:              "gemini-3.5-flash-lite",
			InstructionVersion: "reader-v1",
			UserText:           "test",
		})
		if err == nil {
			t.Fatal("expected error on 400, got nil")
		}
		if strings.Contains(err.Error(), fakeKey) {
			t.Fatalf("API key leaked in error text: %s", err.Error())
		}
	})
}
