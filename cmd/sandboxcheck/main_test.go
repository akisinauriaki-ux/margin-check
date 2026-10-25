package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestRunWithStep3Blocked500AndPlanBDraftMasksIDsAndHidesSecrets(t *testing.T) {
	clearPayPalEnvForTest(t)

	const (
		testClientID = "TEST_CID_SECRET_9001"
		testSecret   = "TEST_CSEC_SECRET_9002"
		testToken    = "TEST_BEARER_SECRET_9003"
		sellerEmail  = "us-seller-test@example.com"
		buyerEmail   = "us-buyer-test@example.com"
		fullInvID    = "INV2-1111-2222-3333-9876"
		planBInvID   = "INV2-4444-5555-6666-4321"
	)

	var (
		mu       sync.Mutex
		byInvNum = map[string]string{}
		invoices []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"` + testToken + `","token_type":"Bearer","expires_in":3600}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/invoices":
			var payload struct {
				Detail struct {
					InvoiceNumber string `json:"invoice_number"`
				} `json:"detail"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			invNum := payload.Detail.InvoiceNumber
			if _, ok := byInvNum[invNum]; ok {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"DUPLICATE_INVOICE_NUMBER"}]}`))
				return
			}
			id := fullInvID
			if len(invoices) >= 1 {
				id = planBInvID
			}
			byInvNum[invNum] = id
			invoices = append(invoices, id)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"` + id + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_number":"` + invNum + `","invoice_date":"2026-10-25"}}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/search-invoices":
			var searchReq struct {
				InvoiceNumber string `json:"invoice_number"`
			}
			_ = json.NewDecoder(r.Body).Decode(&searchReq)
			id := byInvNum[searchReq.InvoiceNumber]
			_, _ = w.Write([]byte(`{"total_items":1,"total_pages":1,"items":[{"id":"` + id + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_number":"` + searchReq.InvoiceNumber + `","invoice_date":"2026-10-25"}}]}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/invoices/"+fullInvID+"/conditional-rules":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"name":"INTERNAL_SERVER_ERROR","message":"An internal server error has occurred.","debug_id":"f297006f25a7d"}`))

		case r.Method == http.MethodGet && r.URL.Path == "/v2/invoicing/invoices/"+fullInvID:
			_, _ = w.Write([]byte(`{"id":"` + fullInvID + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_date":"2026-10-25"}}`))

		case r.Method == http.MethodGet && r.URL.Path == "/v2/invoicing/invoices/"+planBInvID:
			_, _ = w.Write([]byte(`{"id":"` + planBInvID + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_date":"2026-10-25","payment_term":{"term_type":"DUE_ON_DATE_SPECIFIED","due_date":"2026-11-01"}},"items":[{"name":"Wholesale Boxes","quantity":"40","unit_amount":{"currency_code":"USD","value":"15.00"},"discount":{"percent":"10"}}]}`))

		case r.Method == http.MethodGet && r.URL.Path == "/v2/invoicing/invoices":
			_, _ = w.Write([]byte(`{"total_items":1,"total_pages":1,"items":[{"id":"` + fullInvID + `","status":"DRAFT","detail":{"currency_code":"USD"}}]}`))

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
	if len(lines) != 7 {
		t.Fatalf("expected 7 lines (steps 1-6 + plan-B), got %d:\n%s", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[2], "BLOCKED step 3") || !strings.Contains(lines[2], "f297006f25a7d") {
		t.Fatalf("expected line 3 to be BLOCKED step 3 with debug id, got: %q", lines[2])
	}
	if !strings.HasPrefix(lines[6], "PASS plan-B") || !strings.Contains(lines[6], "...4321") {
		t.Fatalf("expected line 7 to be PASS plan-B with ...4321, got: %q", lines[6])
	}

	for _, forbidden := range []string{testClientID, testSecret, testToken, sellerEmail, buyerEmail, fullInvID, planBInvID, "INV2-1111", "INV2-4444"} {
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
