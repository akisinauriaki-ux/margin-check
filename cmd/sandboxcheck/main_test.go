package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"margincheck/internal/paypal"
)

func clearPayPalEnvForTest(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PAYPAL_CLIENT_ID",
		"PAYPAL_CLIENT_SECRET",
		"PAYPAL_API_BASE",
		"PAYPAL_SANDBOX_SELLER_EMAIL",
		"PAYPAL_SANDBOX_BUYER_EMAIL",
	} {
		t.Setenv(k, "")
	}
}

func TestMissingOrBlankDotEnvPrintsExactMessageAndStops(t *testing.T) {
	clearPayPalEnvForTest(t)
	tmp := t.TempDir()
	reportPath := filepath.Join(tmp, "reports", "day1-sandbox.txt")

	var out bytes.Buffer
	code := run(context.Background(), filepath.Join(tmp, "nonexistent.env"), reportPath, nil, &out)
	if code != 0 {
		t.Fatalf("expected clean exit code 0 when .env is missing, got %d", code)
	}
	if strings.TrimSpace(out.String()) != paypal.MissingEnvMessage {
		t.Fatalf("got %q, want %q", strings.TrimSpace(out.String()), paypal.MissingEnvMessage)
	}
	if _, err := os.Stat(reportPath); !os.IsNotExist(err) {
		t.Fatal("expected no partial report file when .env is missing")
	}

	// Blank keys (copied .env.example) must also print the exact message and stop.
	blankEnv := filepath.Join(tmp, "blank.env")
	if err := os.WriteFile(blankEnv, []byte("PAYPAL_CLIENT_ID=\nPAYPAL_CLIENT_SECRET=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	code = run(context.Background(), blankEnv, reportPath, nil, &out)
	if code != 0 || strings.TrimSpace(out.String()) != paypal.MissingEnvMessage {
		t.Fatalf("blank .env got code=%d out=%q", code, out.String())
	}
}

func TestFullSixStepRunMasksIDsAndHidesSecrets(t *testing.T) {
	clearPayPalEnvForTest(t)

	const (
		testClientID = "TEST_CID_SECRET_9001"
		testSecret   = "TEST_CSEC_SECRET_9002"
		testToken    = "TEST_BEARER_SECRET_9003"
		sellerEmail  = "us-seller-test@example.com"
		buyerEmail   = "us-buyer-test@example.com"
		fullInvID    = "INV2-1111-2222-3333-9876"
	)

	var (
		mu       sync.Mutex
		byReqID  = map[string]string{}
		invoices []string
		hasRule  bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"` + testToken + `","token_type":"Bearer","expires_in":3600}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/invoices":
			reqID := r.Header.Get("PayPal-Request-Id")
			if existing, ok := byReqID[reqID]; ok {
				_, _ = w.Write([]byte(`{"id":"` + existing + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_date":"2026-10-25"}}`))
				return
			}
			byReqID[reqID] = fullInvID
			invoices = append(invoices, fullInvID)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"` + fullInvID + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_date":"2026-10-25"}}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/invoices/"+fullInvID+"/conditional-rules":
			hasRule = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"rules":[{"conditional_rule_id":"CR-9999","conditional_rule_type":"EARLY_PAYMENT_DISCOUNT","conditional_rule_value_type":"PERCENT","conditional_rule_value":"10","rule_expiry_terms":{"rule_expiry_condition":"SEVEN_DAYS_AFTER_ISSUE_DATE","condition_rule_end_date":"2026-11-01"}}]}`))

		case r.Method == http.MethodGet && r.URL.Path == "/v2/invoicing/invoices/"+fullInvID:
			if hasRule {
				_, _ = w.Write([]byte(`{"id":"` + fullInvID + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_date":"2026-10-25"},"configuration":{"has_conditional_rule":true}}`))
			} else {
				_, _ = w.Write([]byte(`{"id":"` + fullInvID + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_date":"2026-10-25"}}`))
			}

		case r.Method == http.MethodGet && r.URL.Path == "/v2/invoicing/invoices":
			if len(invoices) == 1 {
				_, _ = w.Write([]byte(`{"total_items":1,"total_pages":1,"items":[{"id":"` + fullInvID + `","status":"DRAFT","detail":{"currency_code":"USD"}}]}`))
			} else {
				_, _ = w.Write([]byte(`{"total_items":2,"total_pages":1,"items":[]}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tmp := t.TempDir()
	envFile := filepath.Join(tmp, "fake.env")
	envContent := strings.Join([]string{
		"PAYPAL_CLIENT_ID=" + testClientID,
		"PAYPAL_CLIENT_SECRET=" + testSecret,
		"PAYPAL_API_BASE=" + srv.URL,
		"PAYPAL_SANDBOX_SELLER_EMAIL=" + sellerEmail,
		"PAYPAL_SANDBOX_BUYER_EMAIL=" + buyerEmail,
	}, "\n")
	if err := os.WriteFile(envFile, []byte(envContent), 0o600); err != nil {
		t.Fatal(err)
	}

	reportPath := filepath.Join(tmp, "reports", "day1-sandbox.txt")
	var out bytes.Buffer
	code := run(context.Background(), envFile, reportPath, srv.Client(), &out)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d:\n%s", code, out.String())
	}

	saved, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read saved report: %v", err)
	}
	if string(saved) != out.String() {
		t.Fatalf("saved report mismatch:\nsaved:\n%s\nout:\n%s", string(saved), out.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected 6 PASS lines, got %d:\n%s", len(lines), out.String())
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, "PASS ") {
			t.Fatalf("line %d did not start with PASS: %q", i+1, l)
		}
	}
	if !strings.Contains(out.String(), "...9876") {
		t.Fatalf("expected masked last-4 invoice ID ...9876 in output:\n%s", out.String())
	}

	for _, forbidden := range []string{testClientID, testSecret, testToken, sellerEmail, buyerEmail, fullInvID, "INV2-1111"} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("forbidden value %q appeared in output:\n%s", forbidden, out.String())
		}
	}
}

func TestStopRuleHaltsOnFirstFailedSandboxStep(t *testing.T) {
	clearPayPalEnvForTest(t)

	callsAfterFail := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"tok_stop","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/invoices":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"PAYEE_ACCOUNT_INVALID"}]}`))
		default:
			callsAfterFail++
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	tmp := t.TempDir()
	envFile := filepath.Join(tmp, "fake.env")
	_ = os.WriteFile(envFile, []byte(strings.Join([]string{
		"PAYPAL_CLIENT_ID=cid",
		"PAYPAL_CLIENT_SECRET=csec",
		"PAYPAL_API_BASE=" + srv.URL,
		"PAYPAL_SANDBOX_SELLER_EMAIL=s@example.com",
		"PAYPAL_SANDBOX_BUYER_EMAIL=b@example.com",
	}, "\n")), 0o600)

	var out bytes.Buffer
	code := run(context.Background(), envFile, filepath.Join(tmp, "reports", "day1-sandbox.txt"), srv.Client(), &out)
	if code == 0 {
		t.Fatal("expected non-zero exit code on failed sandbox step")
	}
	if callsAfterFail != 0 {
		t.Fatalf("expected execution to stop immediately after failed step, got %d extra calls", callsAfterFail)
	}
	if !strings.Contains(out.String(), "FAIL step 2") || !strings.Contains(out.String(), "status=422 error_name=UNPROCESSABLE_ENTITY") {
		t.Fatalf("expected exact status and error name in failure output:\n%s", out.String())
	}
}
