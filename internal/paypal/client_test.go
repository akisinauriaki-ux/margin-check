package paypal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedReq struct {
	Method    string
	Path      string
	Query     string
	RequestID string
	Prefer    string
	Body      string
}

func TestEveryPostCarriesPayPalRequestIDAndEndToEndFlow(t *testing.T) {
	const (
		fakeClientID     = "FAKE_CLIENT_ID_9991"
		fakeClientSecret = "FAKE_SECRET_VALUE_8882"
		fakeToken        = "FAKE_ACCESS_TOKEN_7773"
	)

	var (
		mu       sync.Mutex
		recorded []recordedReq
		tokCalls int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		mu.Lock()
		recorded = append(recorded, recordedReq{
			Method:    r.Method,
			Path:      r.URL.Path,
			Query:     r.URL.RawQuery,
			RequestID: r.Header.Get("PayPal-Request-Id"),
			Prefer:    r.Header.Get("Prefer"),
			Body:      string(bodyBytes),
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/token":
			mu.Lock()
			tokCalls++
			mu.Unlock()
			user, pass, ok := r.BasicAuth()
			if !ok || user != fakeClientID || pass != fakeClientSecret {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad auth"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"` + fakeToken + `","token_type":"Bearer","app_id":"APP-TEST","expires_in":3600}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/invoices":
			if r.Header.Get("Authorization") != "Bearer "+fakeToken {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"name":"AUTHENTICATION_FAILURE"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"INV2-AAAA-BBBB-CCCC-1234","status":"DRAFT","detail":{"currency_code":"USD","invoice_date":"2026-10-25","payment_term":{"term_type":"NET_30"}}}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/invoices/INV2-AAAA-BBBB-CCCC-1234/conditional-rules":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"rules":[{"conditional_rule_id":"CR-1111","conditional_rule_type":"EARLY_PAYMENT_DISCOUNT","conditional_rule_value_type":"PERCENT","conditional_rule_value":"10","rule_expiry_terms":{"rule_expiry_condition":"SEVEN_DAYS_AFTER_ISSUE_DATE","condition_rule_end_date":"2026-11-01"}}]}`))

		case r.Method == http.MethodGet && r.URL.Path == "/v2/invoicing/invoices/INV2-AAAA-BBBB-CCCC-1234":
			_, _ = w.Write([]byte(`{"id":"INV2-AAAA-BBBB-CCCC-1234","status":"DRAFT","detail":{"currency_code":"USD","invoice_date":"2026-10-25","payment_term":{"term_type":"NET_30","conditional_rules":{"early_payment_discount":{"percent":"10","discount_end_date":"2026-11-01"}}}},"configuration":{"has_conditional_rule":true}}`))

		case r.Method == http.MethodGet && r.URL.Path == "/v2/invoicing/invoices":
			_, _ = w.Write([]byte(`{"total_items":1,"total_pages":1,"items":[{"id":"INV2-AAAA-BBBB-CCCC-1234","status":"DRAFT","detail":{"currency_code":"USD"}}]}`))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, fakeClientID, fakeClientSecret, srv.Client())
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx := context.Background()

	// Empty PayPal-Request-Id must be rejected before making any network call.
	if err := client.EnsureToken(ctx, "   "); err == nil {
		t.Fatal("expected error for empty token requestID")
	}
	if _, err := client.CreateDraftInvoice(ctx, "", DraftInvoiceInput{
		CurrencyCode: "USD",
		SellerEmail:  "seller@example.com",
		BuyerEmail:   "buyer@example.com",
		Items:        []InvoiceItemInput{{Name: "Boxes", Quantity: 40, UnitPriceCents: 1500}},
	}); err == nil {
		t.Fatal("expected error for empty invoice requestID")
	}
	if _, err := client.CreateEarlyPaymentDiscountRule(ctx, "", EarlyPaymentDiscountInput{
		InvoiceID: "INV2-AAAA-BBBB-CCCC-1234",
		Percent:   10,
	}); err == nil {
		t.Fatal("expected error for empty conditional-rule requestID")
	}
	if len(recorded) != 0 {
		t.Fatalf("expected 0 HTTP calls when requestID is missing, got %d", len(recorded))
	}

	if err := client.EnsureToken(ctx, "req-token-1"); err != nil {
		t.Fatalf("EnsureToken failed: %v", err)
	}

	inv, err := client.CreateDraftInvoice(ctx, "req-inv-1", DraftInvoiceInput{
		CurrencyCode:    "USD",
		SellerEmail:     "seller@example.com",
		BuyerEmail:      "buyer@example.com",
		PaymentTermType: PaymentTermNet30,
		Items: []InvoiceItemInput{
			{Name: "Wholesale Boxes", Quantity: 40, UnitPriceCents: 1525},
		},
	})
	if err != nil {
		t.Fatalf("CreateDraftInvoice failed: %v", err)
	}
	if inv.ID != "INV2-AAAA-BBBB-CCCC-1234" || inv.Status != "DRAFT" {
		t.Fatalf("unexpected invoice: %+v", inv)
	}

	rulesResp, err := client.CreateEarlyPaymentDiscountRule(ctx, "req-rule-1", EarlyPaymentDiscountInput{
		InvoiceID:       inv.ID,
		Percent:         10,
		ExpiryCondition: ExpirySevenDaysAfterIssueDate,
		EndDate:         "2026-11-01",
	})
	if err != nil {
		t.Fatalf("CreateEarlyPaymentDiscountRule failed: %v", err)
	}
	if len(rulesResp.Rules) != 1 || rulesResp.Rules[0].ConditionalRuleValue != "10" {
		t.Fatalf("unexpected rules response: %+v", rulesResp)
	}

	gotInv, err := client.GetInvoice(ctx, inv.ID)
	if err != nil {
		t.Fatalf("GetInvoice failed: %v", err)
	}
	if !gotInv.HasEarlyPaymentDiscount() {
		t.Fatal("expected HasEarlyPaymentDiscount() to be true")
	}

	list, err := client.ListInvoices(ctx, 1, 20, true)
	if err != nil {
		t.Fatalf("ListInvoices failed: %v", err)
	}
	if list.TotalItems != 1 {
		t.Fatalf("expected TotalItems=1, got %d", list.TotalItems)
	}

	// Also prove an unprimed client auto-fetching a token still puts PayPal-Request-Id on the token POST.
	client2, err := NewClient(srv.URL, fakeClientID, fakeClientSecret, srv.Client())
	if err != nil {
		t.Fatalf("NewClient 2 failed: %v", err)
	}
	if _, err := client2.CreateDraftInvoice(ctx, "req-inv-2", DraftInvoiceInput{
		CurrencyCode: "USD",
		SellerEmail:  "seller@example.com",
		BuyerEmail:   "buyer@example.com",
		Items:        []InvoiceItemInput{{Name: "Wholesale Boxes", Quantity: 40, UnitPriceCents: 1500}},
	}); err != nil {
		t.Fatalf("client2 CreateDraftInvoice failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if tokCalls != 2 {
		t.Fatalf("expected token cached in memory (1 fetch per client = 2 total), got %d", tokCalls)
	}

	postCount := 0
	for i, rec := range recorded {
		if rec.Method == http.MethodPost {
			postCount++
			if strings.TrimSpace(rec.RequestID) == "" {
				t.Fatalf("POST request #%d to %s missing PayPal-Request-Id header", i, rec.Path)
			}
		}
		if rec.Method == http.MethodPost && rec.Path == "/v2/invoicing/invoices" {
			if rec.Prefer != "return=representation" {
				t.Fatalf("expected Prefer: return=representation, got %q", rec.Prefer)
			}
		}
	}
	if postCount != 5 {
		t.Fatalf("expected 5 POST calls total, got %d", postCount)
	}
	if recorded[0].RequestID != "req-token-1" || recorded[1].RequestID != "req-inv-1" || recorded[2].RequestID != "req-rule-1" {
		t.Fatalf("caller PayPal-Request-Id values were not preserved: %+v", recorded[:3])
	}

	// Verify whole-cents integer formatting in JSON payload ("15.25" for 1525 cents).
	var sent map[string]any
	if err := json.Unmarshal([]byte(recorded[1].Body), &sent); err != nil {
		t.Fatalf("unmarshal sent invoice payload: %v", err)
	}
	items := sent["items"].([]any)
	unitAmt := items[0].(map[string]any)["unit_amount"].(map[string]any)
	if unitAmt["value"] != "15.25" || unitAmt["currency_code"] != "USD" {
		t.Fatalf("unexpected unit_amount: %+v", unitAmt)
	}
}

func TestTokenAndSecretNeverAppearInErrorsOrLogs(t *testing.T) {
	const (
		secretClientID = "SUPER_SENSITIVE_ID_4411"
		secretValue    = "SUPER_SENSITIVE_SECRET_5522"
		secretToken    = "SUPER_SENSITIVE_BEARER_TOKEN_6633"
	)
	basicB64 := base64.StdEncoding.EncodeToString([]byte(secretClientID + ":" + secretValue))

	var logBuf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origOut)

	mode := "oauth_fail"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch mode {
		case "oauth_fail":
			w.WriteHeader(http.StatusUnauthorized)
			// Deliberately echo secrets back in error fields to prove client scrubs them.
			_, _ = fmt.Fprintf(w, `{"error":"invalid_client_%s_%s","error_description":"Header %s"}`,
				secretClientID, secretValue, r.Header.Get("Authorization"))
		case "oauth_ok_then_422":
			if r.URL.Path == "/v1/oauth2/token" {
				_, _ = fmt.Fprintf(w, `{"access_token":"%s","token_type":"Bearer","expires_in":3600}`, secretToken)
				return
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprintf(w, `{"name":"UNPROCESSABLE_ENTITY","message":"bad token %s secret %s","details":[{"issue":"ISSUE_WITH_%s"}]}`,
				secretToken, secretValue, secretToken)
		}
	}))

	client, err := NewClient(srv.URL, secretClientID, secretValue, srv.Client())
	if err != nil {
		srv.Close()
		t.Fatalf("NewClient failed: %v", err)
	}

	assertNoSecrets := func(label string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected error, got nil", label)
		}
		combined := err.Error() + "\n" + fmt.Sprintf("%+v", err) + "\n" + logBuf.String()
		for _, forbidden := range []string{secretClientID, secretValue, secretToken, basicB64} {
			if strings.Contains(combined, forbidden) {
				t.Fatalf("%s: secret leaked into error or log text", label)
			}
		}
	}

	// Case 1: OAuth failure carrying secrets in response body.
	err = client.EnsureToken(context.Background(), "req-err-1")
	assertNoSecrets("oauth_fail", err)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected APIError with status 401, got %T (%v)", err, err)
	}

	// Case 2: Token cached, subsequent Invoicing 422 response echoes token & secret.
	mode = "oauth_ok_then_422"
	_, err = client.CreateDraftInvoice(context.Background(), "req-err-2", DraftInvoiceInput{
		CurrencyCode: "USD",
		SellerEmail:  "seller@example.com",
		BuyerEmail:   "buyer@example.com",
		Items:        []InvoiceItemInput{{Name: "Boxes", Quantity: 40, UnitPriceCents: 1500}},
	})
	assertNoSecrets("invoicing_422", err)
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity || apiErr.Name != "UNPROCESSABLE_ENTITY" {
		t.Fatalf("expected APIError 422 UNPROCESSABLE_ENTITY, got %+v", err)
	}

	// Case 3: Transport error after server closes.
	srv.Close()
	_, err = client.GetInvoice(context.Background(), "INV2-AAAA-BBBB-CCCC-1234")
	assertNoSecrets("transport_closed", err)
}

func TestSandboxOnlyAndWholeCentsHelpers(t *testing.T) {
	if _, err := NewClient("https://api-m.paypal.com", "id", "secret", nil); err == nil {
		t.Fatal("expected live PayPal URL to be rejected")
	}
	if _, err := NewClient("https://example.com", "id", "secret", nil); err == nil {
		t.Fatal("expected arbitrary remote URL to be rejected")
	}
	if _, err := NewClient(SandboxAPIBase, "id", "secret", nil); err != nil {
		t.Fatalf("expected SandboxAPIBase to be accepted, got: %v", err)
	}

	cases := []struct {
		cents int64
		want  string
	}{
		{0, "0.00"},
		{7, "0.07"},
		{99, "0.99"},
		{1500, "15.00"},
		{123456, "1234.56"},
	}
	for _, tc := range cases {
		got, err := FormatCents(tc.cents)
		if err != nil || got != tc.want {
			t.Fatalf("FormatCents(%d) = (%q, %v), want (%q, nil)", tc.cents, got, err, tc.want)
		}
	}
	if _, err := FormatCents(-50); err == nil {
		t.Fatal("expected negative cents to fail")
	}

	if got := MaskID("INV2-ABCD-EFGH-IJKL-7890"); got != "...7890" {
		t.Fatalf("MaskID got %q, want ...7890", got)
	}
}

func TestConditionalRule204AndHrefFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"tok_abc","token_type":"Bearer","expires_in":1}`))
		case r.URL.Path == "/v2/invoicing/invoices":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"rel":"self","href":"https://api-m.sandbox.paypal.com/v2/invoicing/invoices/INV2-WWWW-XXXX-YYYY-5678","method":"GET"}`))
		case strings.HasSuffix(r.URL.Path, "/conditional-rules"):
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	now := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	client, err := NewClient(srv.URL, "cid", "csec", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.now = func() time.Time { return now }

	inv, err := client.CreateDraftInvoice(context.Background(), "req-href", DraftInvoiceInput{
		CurrencyCode: "USD",
		SellerEmail:  "s@example.com",
		BuyerEmail:   "b@example.com",
		Items:        []InvoiceItemInput{{Name: "Boxes", Quantity: 40, UnitPriceCents: 1500}},
	})
	if err != nil {
		t.Fatalf("CreateDraftInvoice href fallback failed: %v", err)
	}
	if inv.ID != "INV2-WWWW-XXXX-YYYY-5678" {
		t.Fatalf("expected extracted ID INV2-WWWW-XXXX-YYYY-5678, got %q", inv.ID)
	}

	if _, err := client.CreateEarlyPaymentDiscountRule(context.Background(), "req-204", EarlyPaymentDiscountInput{
		InvoiceID: inv.ID,
		Percent:   10,
	}); err != nil {
		t.Fatalf("expected 204 No Content to succeed, got: %v", err)
	}
}

func TestRepeatCreateDraftInvoiceUsesDeterministicInvoiceNumberAndRecoversOn422Duplicate(t *testing.T) {
	const (
		fakeClientID     = "FAKE_CLIENT_ID_DUP"
		fakeClientSecret = "FAKE_SECRET_DUP"
		fakeToken        = "FAKE_TOKEN_DUP"
		firstInvoiceID   = "INV2-DUP1-DUP2-DUP3-7788"
		secondInvoiceID  = "INV2-NEW1-NEW2-NEW3-9900"
	)

	var (
		mu             sync.Mutex
		byInvNum       = map[string]string{}
		createdNumbers []string
		searchCalls    int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"` + fakeToken + `","token_type":"Bearer","expires_in":3600}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/invoices":
			// Simulate real PayPal Sandbox: ignore PayPal-Request-Id, enforce unique detail.invoice_number.
			var payload struct {
				Detail struct {
					InvoiceNumber string `json:"invoice_number"`
				} `json:"detail"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			invNum := payload.Detail.InvoiceNumber
			if invNum == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"name":"INVALID_REQUEST"}`))
				return
			}
			if len(invNum) > MaxInvoiceNumberLen {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"name":"INVALID_STRING_LENGTH"}`))
				return
			}
			if _, exists := byInvNum[invNum]; exists {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"name":"UNPROCESSABLE_ENTITY","message":"The requested action could not be performed, semantically incorrect, or failed business validation.","details":[{"issue":"DUPLICATE_INVOICE_NUMBER","description":"Invoice number already exists."}]}`))
				return
			}
			id := firstInvoiceID
			if len(byInvNum) >= 1 {
				id = secondInvoiceID
			}
			byInvNum[invNum] = id
			createdNumbers = append(createdNumbers, invNum)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"` + id + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_number":"` + invNum + `","invoice_date":"2026-10-25"}}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/search-invoices":
			searchCalls++
			if strings.TrimSpace(r.Header.Get("PayPal-Request-Id")) == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var searchReq struct {
				InvoiceNumber string `json:"invoice_number"`
			}
			_ = json.NewDecoder(r.Body).Decode(&searchReq)
			id, ok := byInvNum[searchReq.InvoiceNumber]
			if !ok {
				_, _ = w.Write([]byte(`{"total_items":0,"total_pages":0,"items":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"total_items":1,"total_pages":1,"items":[{"id":"` + id + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_number":"` + searchReq.InvoiceNumber + `","invoice_date":"2026-10-25"}}]}`))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, fakeClientID, fakeClientSecret, srv.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx := context.Background()
	in := DraftInvoiceInput{
		CurrencyCode:    "USD",
		SellerEmail:     "seller@example.com",
		BuyerEmail:      "buyer@example.com",
		PaymentTermType: PaymentTermNet30,
		Items:           []InvoiceItemInput{{Name: "Wholesale Boxes", Quantity: 40, UnitPriceCents: 1500}},
	}

	// (a) First call creates the draft; repeat with the same key receives 422 DUPLICATE_INVOICE_NUMBER,
	// searches by detail.invoice_number, and returns the first invoice's ID marked AlreadyExisted=true.
	first, err := client.CreateDraftInvoice(ctx, "idem-key-alpha", in)
	if err != nil {
		t.Fatalf("first CreateDraftInvoice failed: %v", err)
	}
	if first.ID != firstInvoiceID || first.AlreadyExisted {
		t.Fatalf("unexpected first invoice: %+v", first)
	}

	repeat, err := client.CreateDraftInvoice(ctx, "idem-key-alpha", in)
	if err != nil {
		t.Fatalf("repeat CreateDraftInvoice failed: %v", err)
	}
	if repeat.ID != first.ID {
		t.Fatalf("expected repeat ID %q to match first ID %q", repeat.ID, first.ID)
	}
	if !repeat.AlreadyExisted {
		t.Fatal("expected repeat invoice to have AlreadyExisted=true")
	}
	if repeat.Detail.InvoiceNumber != first.Detail.InvoiceNumber {
		t.Fatalf("expected same derived invoice_number, got %q vs %q", repeat.Detail.InvoiceNumber, first.Detail.InvoiceNumber)
	}

	// (b) Different keys give different invoice numbers (each <= 25 chars) and create a separate draft.
	second, err := client.CreateDraftInvoice(ctx, "idem-key-beta", in)
	if err != nil {
		t.Fatalf("second CreateDraftInvoice failed: %v", err)
	}
	if second.ID != secondInvoiceID || second.AlreadyExisted {
		t.Fatalf("unexpected second invoice: %+v", second)
	}
	if second.Detail.InvoiceNumber == first.Detail.InvoiceNumber {
		t.Fatalf("different keys produced identical invoice_number %q", second.Detail.InvoiceNumber)
	}
	if len(first.Detail.InvoiceNumber) > MaxInvoiceNumberLen || len(second.Detail.InvoiceNumber) > MaxInvoiceNumberLen {
		t.Fatalf("invoice_number exceeded %d chars: %q, %q", MaxInvoiceNumberLen, first.Detail.InvoiceNumber, second.Detail.InvoiceNumber)
	}

	mu.Lock()
	if len(createdNumbers) != 2 || searchCalls != 1 {
		t.Fatalf("expected 2 created invoices and 1 search call, got created=%d searchCalls=%d", len(createdNumbers), searchCalls)
	}
	mu.Unlock()

	// (c) Prove the derived invoice_number never contains a secret even if secret material is passed.
	const rawSecret = "TOP_SECRET_TOKEN_XYZ_98765"
	derived, err := DeriveInvoiceNumber("key-with-" + rawSecret + "-" + fakeClientSecret)
	if err != nil {
		t.Fatalf("DeriveInvoiceNumber failed: %v", err)
	}
	if len(derived) > MaxInvoiceNumberLen {
		t.Fatalf("DeriveInvoiceNumber length %d > %d", len(derived), MaxInvoiceNumberLen)
	}
	for _, secret := range []string{rawSecret, "TOP_SECRET", "XYZ_98765", fakeClientSecret, fakeClientID, fakeToken} {
		if strings.Contains(strings.ToUpper(derived), strings.ToUpper(secret)) {
			t.Fatalf("derived invoice_number %q leaked secret %q", derived, secret)
		}
	}
}
