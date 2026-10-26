package reader

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"margincheck/internal/gemini"
)

type fakeGenerator struct {
	calls     []gemini.Request
	responses []gemini.Response
}

func (f *fakeGenerator) Generate(_ context.Context, req gemini.Request) (gemini.Response, error) {
	idx := len(f.calls)
	f.calls = append(f.calls, req)
	if idx < len(f.responses) {
		return f.responses[idx], nil
	}
	return f.responses[len(f.responses)-1], nil
}

var reWord = regexp.MustCompile(`\w+`)

func extractWords(s string) []string {
	matches := reWord.FindAllString(strings.ToLower(s), -1)
	return matches
}

func TestNoEightWordOverlapBetweenEmailsAndPrompts(t *testing.T) {
	promptFiles, err := filepath.Glob(filepath.Join("..", "..", "prompts", "*.md"))
	if err != nil {
		t.Fatalf("glob prompts: %v", err)
	}
	if len(promptFiles) == 0 {
		t.Fatalf("no prompt files found in ../../prompts/")
	}

	emailFiles, err := filepath.Glob(filepath.Join("..", "..", "testdata", "emails", "*.txt"))
	if err != nil {
		t.Fatalf("glob emails: %v", err)
	}
	if len(emailFiles) != 20 {
		t.Fatalf("expected 20 email files, found %d", len(emailFiles))
	}

	for _, pf := range promptFiles {
		pBytes, err := os.ReadFile(pf)
		if err != nil {
			t.Fatalf("read prompt %s: %v", pf, err)
		}
		pWords := extractWords(string(pBytes))
		ngrams := make(map[string]bool)
		for i := 0; i+8 <= len(pWords); i++ {
			ngrams[strings.Join(pWords[i:i+8], " ")] = true
		}

		for _, ef := range emailFiles {
			eBytes, err := os.ReadFile(ef)
			if err != nil {
				t.Fatalf("read email %s: %v", ef, err)
			}
			eWords := extractWords(string(eBytes))
			for i := 0; i+8 <= len(eWords); i++ {
				seq := strings.Join(eWords[i:i+8], " ")
				if ngrams[seq] {
					t.Errorf("8-word overlap between %s and %s: %q", filepath.Base(pf), filepath.Base(ef), seq)
				}
			}
		}
	}
}

func TestWorkedExamplesInPromptPassVerification(t *testing.T) {
	promptBytes, err := os.ReadFile(filepath.Join("..", "..", "prompts", "reader-v1.md"))
	if err != nil {
		t.Fatalf("read reader-v1.md: %v", err)
	}
	text := string(promptBytes)

	reExample := regexp.MustCompile("(?s)Input email:\\s*```text\\s*(.*?)\\s*```\\s*Expected JSON output:\\s*```json\\s*(\\{.*?\\})\\s*```")
	matches := reExample.FindAllStringSubmatch(text, -1)
	if len(matches) < 3 {
		t.Fatalf("expected at least 3 worked examples in reader-v1.md, found %d", len(matches))
	}

	for i, m := range matches {
		email := m[1]
		jsonStr := m[2]
		var raw RawReading
		if err := strictUnmarshalReading(jsonStr, &raw); err != nil {
			t.Fatalf("example %d JSON unmarshal failed: %v", i+1, err)
		}
		vr := verifySingleReading(email, raw)
		if len(vr.verifierErrors) > 0 {
			t.Errorf("example %d had verifier errors: %v", i+1, vr.verifierErrors)
		}
	}
}

func TestPIIMaskingAndUnmasking(t *testing.T) {
	rawEmail := `Based on: synthetic test

Buyer:
Hi, reach me at buyer@acme-candles.org or (415) 555-0199 or https://acme-candles.org/orders.
We want 20 boxes at $40 per box, payment due on receipt and $24 shipping.`

	validReply := `{
  "quantity": {"value": "20", "source_sentence": "We want 20 boxes at $40 per box, payment due on receipt and $24 shipping."},
  "unit_price_cents": {"value": "4000", "source_sentence": "We want 20 boxes at $40 per box, payment due on receipt and $24 shipping."},
  "discount_percent": {"value": "0", "source_sentence": ""},
  "free_shipping": {"value": "false", "source_sentence": "We want 20 boxes at $40 per box, payment due on receipt and $24 shipping."},
  "payment_days": {"value": "0", "source_sentence": "We want 20 boxes at $40 per box, payment due on receipt and $24 shipping."},
  "rebate_percent": {"value": "0", "source_sentence": ""},
  "price_increase_discounted": {"value": "false", "source_sentence": ""},
  "accepted_option": {"value": "", "source_sentence": ""},
  "must_ask": [],
  "terms": [],
  "buyer_interest": {
    "summary": "Contact buyer at [EMAIL_1] or [PHONE_1] or [URL_1].",
    "source_sentences": ["Hi, reach me at [EMAIL_1] or [PHONE_1] or [URL_1]."]
  }
}`

	fg := &fakeGenerator{
		responses: []gemini.Response{
			{Text: validReply, ModelVersion: "model-a-v1", TotalTokens: 500},
			{Text: validReply, ModelVersion: "model-b-v1", TotalTokens: 500},
		},
	}

	traceDir := t.TempDir()
	r, err := New(Config{
		Client:     fg,
		PromptPath: filepath.Join("..", "..", "prompts", "reader-v1.md"),
		ModelA:     "model-a",
		ModelB:     "model-b",
		TraceDir:   traceDir,
	})
	if err != nil {
		t.Fatalf("New reader: %v", err)
	}

	res, err := r.Check(context.Background(), "pii-test", rawEmail)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}

	// Verify the prompt sent to Gemini had PII masked, Based on: stripped, and <untrusted_email> tags.
	sentPrompt := fg.calls[0].UserText
	if strings.Contains(sentPrompt, "buyer@acme-candles.org") {
		t.Errorf("email PII leaked into model prompt: %s", sentPrompt)
	}
	if strings.Contains(sentPrompt, "555-0199") {
		t.Errorf("phone PII leaked into model prompt: %s", sentPrompt)
	}
	if strings.Contains(sentPrompt, "https://acme-candles.org") {
		t.Errorf("URL PII leaked into model prompt: %s", sentPrompt)
	}
	if strings.Contains(sentPrompt, "Based on:") {
		t.Errorf("Based on: metadata line was not stripped: %s", sentPrompt)
	}
	if !strings.Contains(sentPrompt, "<untrusted_email>") || !strings.Contains(sentPrompt, "</untrusted_email>") {
		t.Errorf("missing <untrusted_email> tags in prompt: %s", sentPrompt)
	}

	// Verify PII was restored in the returned result and verified against original email.
	if len(res.BuyerInterest.SourceSentences) != 1 || !strings.Contains(res.BuyerInterest.SourceSentences[0], "buyer@acme-candles.org") {
		t.Errorf("PII was not unmasked in BuyerInterest.SourceSentences: %+v", res.BuyerInterest.SourceSentences)
	}
	if !res.Quantity.Sure || res.Quantity.Value != 20 {
		t.Errorf("expected sure quantity 20, got %+v", res.Quantity)
	}
	if res.TracePath == "" {
		t.Errorf("expected trace file to be written")
	}
}

func TestDisagreementRetryAndBudget(t *testing.T) {
	email := `Buyer:
We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days.`

	badQuoteReply := `{
  "quantity": {"value": "20", "source_sentence": "Invented sentence not in email."},
  "unit_price_cents": {"value": "4000", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "discount_percent": {"value": "10", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "free_shipping": {"value": "true", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "payment_days": {"value": "30", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "rebate_percent": {"value": "0", "source_sentence": ""},
  "price_increase_discounted": {"value": "false", "source_sentence": ""},
  "accepted_option": {"value": "", "source_sentence": ""},
  "must_ask": [],
  "terms": [],
  "buyer_interest": {"summary": "Order", "source_sentences": []}
}`

	fixedGoodReply := `{
  "quantity": {"value": "20", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "unit_price_cents": {"value": "4000", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "discount_percent": {"value": "10", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "free_shipping": {"value": "true", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "payment_days": {"value": "30", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "rebate_percent": {"value": "0", "source_sentence": ""},
  "price_increase_discounted": {"value": "false", "source_sentence": ""},
  "accepted_option": {"value": "", "source_sentence": ""},
  "must_ask": [],
  "terms": [],
  "buyer_interest": {"summary": "Order", "source_sentences": []}
}`

	disagreeDiscountReply := `{
  "quantity": {"value": "20", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "unit_price_cents": {"value": "4000", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "discount_percent": {"value": "0", "source_sentence": ""},
  "free_shipping": {"value": "true", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "payment_days": {"value": "30", "source_sentence": "We want 20 boxes at $40 per box with a 10% discount, free shipping, and net 30 days."},
  "rebate_percent": {"value": "0", "source_sentence": ""},
  "price_increase_discounted": {"value": "false", "source_sentence": ""},
  "accepted_option": {"value": "", "source_sentence": ""},
  "must_ask": [],
  "terms": [],
  "buyer_interest": {"summary": "Order", "source_sentences": []}
}`

	// Model A fails quote on attempt 1, fixes it on attempt 2.
	// Model B disagrees on discount_percent (0 vs 10).
	fg := &fakeGenerator{
		responses: []gemini.Response{
			{Text: badQuoteReply, TotalTokens: 400},
			{Text: fixedGoodReply, TotalTokens: 400},
			{Text: disagreeDiscountReply, TotalTokens: 400},
		},
	}

	r, err := New(Config{
		Client:     fg,
		PromptPath: filepath.Join("..", "..", "prompts", "reader-v1.md"),
		ModelA:     "model-a",
		ModelB:     "model-b",
	})
	if err != nil {
		t.Fatalf("New reader: %v", err)
	}

	res, err := r.Check(context.Background(), "retry-disagree", email)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.CallsUsed != 3 {
		t.Errorf("expected 3 calls (2 for ModelA retry, 1 for ModelB), got %d", res.CallsUsed)
	}
	if !res.Quantity.Sure || res.Quantity.Value != 20 {
		t.Errorf("expected quantity Sure=true after retry, got %+v", res.Quantity)
	}
	if res.DiscountPercent.Sure || res.DiscountPercent.Reason != ReasonReadingsDisagree {
		t.Errorf("expected discount_percent Sure=false with reason %q, got %+v", ReasonReadingsDisagree, res.DiscountPercent)
	}

	// Test token budget exceeded marks all fields as ask with reason "budget".
	fgBudget := &fakeGenerator{
		responses: []gemini.Response{
			{Text: fixedGoodReply, TotalTokens: 70000},
		},
	}
	rBudget, err := New(Config{
		Client:            fgBudget,
		PromptPath:        filepath.Join("..", "..", "prompts", "reader-v1.md"),
		ModelA:            "model-a",
		ModelB:            "model-b",
		MaxTokensPerCheck: 60000,
	})
	if err != nil {
		t.Fatalf("New budget reader: %v", err)
	}
	resBudget, err := rBudget.Check(context.Background(), "budget-test", email)
	if err != nil {
		t.Fatalf("Check budget: %v", err)
	}
	if !resBudget.BudgetExceeded {
		t.Errorf("expected BudgetExceeded=true")
	}
	if resBudget.Quantity.Sure || resBudget.Quantity.Reason != ReasonBudget {
		t.Errorf("expected Quantity.Reason=%q, got %+v", ReasonBudget, resBudget.Quantity)
	}
}

func TestConfidenceFieldRejected(t *testing.T) {
	var raw RawReading
	err := strictUnmarshalReading(`{"confidence": 0.99}`, &raw)
	if err == nil || !strings.Contains(err.Error(), "confidence") {
		t.Fatalf("expected confidence field to be rejected, got: %v", err)
	}
	_ = json.Marshal
}

func TestVerifyBoolDealFieldEmptyIsCantParse(t *testing.T) {
	cleanEmail := "We will include free shipping on this order."
	var errs []string

	// 1. Empty value must return valid=false and ReasonValueCantParse, not false.
	resEmpty := verifyBoolDealField(cleanEmail, "free_shipping", RawField{
		Value:          "",
		SourceSentence: "We will include free shipping on this order.",
	}, true, false, &errs)

	if resEmpty.valid {
		t.Errorf("expected valid=false for empty boolean, got valid=true (val=%v)", resEmpty.val)
	}
	if resEmpty.reason != ReasonValueCantParse {
		t.Errorf("expected reason=%q for empty boolean, got %q", ReasonValueCantParse, resEmpty.reason)
	}
	if resEmpty.val {
		t.Errorf("expected val=false when unparseable, got %v", resEmpty.val)
	}

	// 2. Whitespace-only value must also be unparseable
	resWhitespace := verifyBoolDealField(cleanEmail, "free_shipping", RawField{
		Value:          "   ",
		SourceSentence: "",
	}, true, false, &errs)
	if resWhitespace.valid || resWhitespace.reason != ReasonValueCantParse {
		t.Errorf("expected whitespace boolean to be unparseable, got valid=%v reason=%q", resWhitespace.valid, resWhitespace.reason)
	}

	// 3. Explicit "false" is valid false
	resFalse := verifyBoolDealField(cleanEmail, "free_shipping", RawField{
		Value:          "false",
		SourceSentence: "",
	}, true, false, &errs)
	if !resFalse.valid || resFalse.val != false {
		t.Errorf("expected valid false, got valid=%v val=%v", resFalse.valid, resFalse.val)
	}

	// 4. Explicit "true" is valid true with backing quote
	resTrue := verifyBoolDealField(cleanEmail, "free_shipping", RawField{
		Value:          "true",
		SourceSentence: "We will include free shipping on this order.",
	}, true, false, &errs)
	if !resTrue.valid || resTrue.val != true {
		t.Errorf("expected valid true, got valid=%v val=%v", resTrue.valid, resTrue.val)
	}
}

func TestUnavailableModelFakeServer(t *testing.T) {
	email := "We want 20 boxes at $40 each."

	t.Run("HTTP 503 results in model_unavailable ask fields", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error": {"code": 503, "message": "Service Unavailable"}}`))
		}))
		defer srv.Close()

		cli, err := gemini.NewClient("fake-key", t.TempDir(), true)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		cli.BaseURL = srv.URL
		cli.MinInterval = 0

		r, err := New(Config{
			Client:     cli,
			PromptPath: filepath.Join("..", "..", "prompts", "reader-v1.md"),
			ModelA:     "test-model",
			Single:     true,
		})
		if err != nil {
			t.Fatalf("New reader: %v", err)
		}

		res, err := r.Check(context.Background(), "unav-503", email)
		if err != nil {
			t.Fatalf("expected Check to succeed and end check gracefully, got error: %v", err)
		}
		if !res.ModelUnavailable || res.UnavailableReason != ReasonModelUnavailable {
			t.Errorf("expected ModelUnavailable=true with reason %q, got %+v", ReasonModelUnavailable, res)
		}
		if res.Quantity.Sure || res.Quantity.Reason != ReasonModelUnavailable {
			t.Errorf("expected Quantity Reason=%q, got %+v", ReasonModelUnavailable, res.Quantity)
		}
		if res.UnitPriceCents.Sure || res.UnitPriceCents.Reason != ReasonModelUnavailable {
			t.Errorf("expected UnitPriceCents Reason=%q, got %+v", ReasonModelUnavailable, res.UnitPriceCents)
		}
	})

	t.Run("daily 429 results in daily_limit ask fields", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		cli, err := gemini.NewClient("fake-key", t.TempDir(), true)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		cli.BaseURL = srv.URL
		cli.MinInterval = 0

		r, err := New(Config{
			Client:     cli,
			PromptPath: filepath.Join("..", "..", "prompts", "reader-v1.md"),
			ModelA:     "test-model",
			Single:     true,
		})
		if err != nil {
			t.Fatalf("New reader: %v", err)
		}

		res, err := r.Check(context.Background(), "unav-daily", email)
		if err != nil {
			t.Fatalf("expected Check to return nil error on daily limit, got: %v", err)
		}
		if !res.DailyLimitHit || res.UnavailableReason != ReasonDailyLimit {
			t.Errorf("expected DailyLimitHit=true with reason %q, got %+v", ReasonDailyLimit, res)
		}
		if res.Quantity.Sure || res.Quantity.Reason != ReasonDailyLimit {
			t.Errorf("expected Quantity Reason=%q, got %+v", ReasonDailyLimit, res.Quantity)
		}
	})

	t.Run("timeout results in model_unavailable ask fields", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
		}))
		defer srv.Close()

		cli, err := gemini.NewClient("fake-key", t.TempDir(), true)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		cli.BaseURL = srv.URL
		cli.MinInterval = 0

		r, err := New(Config{
			Client:             cli,
			PromptPath:         filepath.Join("..", "..", "prompts", "reader-v1.md"),
			ModelA:             "test-model",
			Single:             true,
			MaxSecondsPerCheck: 1,
		})
		if err != nil {
			t.Fatalf("New reader: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		res, err := r.Check(ctx, "unav-timeout", email)
		if err != nil {
			t.Fatalf("expected Check to return nil error on timeout, got: %v", err)
		}
		if !res.ModelUnavailable || res.UnavailableReason != ReasonModelUnavailable {
			t.Errorf("expected ModelUnavailable=true with reason %q, got %+v", ReasonModelUnavailable, res)
		}
		if res.Quantity.Sure || res.Quantity.Reason != ReasonModelUnavailable {
			t.Errorf("expected Quantity Reason=%q, got %+v", ReasonModelUnavailable, res.Quantity)
		}
	})
}
