package writer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"margincheck/internal/gemini"
	"margincheck/internal/offer"
	"margincheck/internal/profit"
)

type callLog struct {
	attempt int
	req     gemini.Request
	resp    gemini.Response
	err     error
}

type recordingClient struct {
	client    *gemini.Client
	callLimit int
	calls     []callLog
}

func (r *recordingClient) Generate(ctx context.Context, req gemini.Request) (gemini.Response, error) {
	if len(r.calls) >= r.callLimit {
		return gemini.Response{}, fmt.Errorf("call limit (%d) reached; aborting to preserve budget", r.callLimit)
	}
	resp, err := r.client.Generate(ctx, req)
	r.calls = append(r.calls, callLog{
		attempt: len(r.calls) + 1,
		req:     req,
		resp:    resp,
		err:     err,
	})
	return resp, err
}

func loadSellerHelper() (profit.Seller, error) {
	paths := []string{"../../testdata/seller.json", "../testdata/seller.json", "testdata/seller.json"}
	for _, p := range paths {
		if s, err := profit.LoadSeller(p); err == nil {
			return s, nil
		}
	}
	return profit.Seller{}, fmt.Errorf("seller.json not found in %v", paths)
}

func loadKeyHelper() (string, error) {
	if k := strings.TrimSpace(os.Getenv("GEMINI_API_KEY")); k != "" {
		return k, nil
	}
	paths := []string{"../../.env", "../.env", ".env"}
	for _, p := range paths {
		if k, err := gemini.LoadAPIKey(p); err == nil && k != "" {
			return k, nil
		}
	}
	return "", fmt.Errorf("GEMINI_API_KEY not found in env or %v", paths)
}

func TestLiveSamples(t *testing.T) {
	if os.Getenv("RUN_LIVE_SAMPLES") != "1" {
		t.Skip("skipping live samples; set RUN_LIVE_SAMPLES=1 to run")
	}

	key, err := loadKeyHelper()
	if err != nil {
		t.Fatalf("load API key: %v", err)
	}

	gemClient, err := gemini.NewClient(key, "", true) // noCache = true for live calls
	if err != nil {
		t.Fatalf("create gemini client: %v", err)
	}

	seller, err := loadSellerHelper()
	if err != nil {
		t.Fatalf("load seller: %v", err)
	}

	recorder := &recordingClient{
		client:    gemClient,
		callLimit: 8, // at most 8 live calls in all
	}

	cases := []struct {
		id     string
		title  string
		thread string
		req    offer.Request
	}{
		{
			id:    "sample",
			title: "Sample thread from cmd/app/main.go",
			thread: "Buyer:\nHi, we want 20 boxes of stoneware mugs at $40 per box for our shop.\nWe need free shipping and two months to pay.\n\nSeller:\nSure, happy to help! Free shipping and 60 days is fine for 20 boxes.",
			req: offer.Request{
				Currency:             "USD",
				Quantity:             20,
				UnitPriceCents:       4000,
				RequestedPaymentDays: 60,
				RequestedDiscountBP:  0,
			},
		},
		{
			id:    "email-01",
			title: "Exam email 01 (testdata/emails/01.txt, wholesale mugs, 40 boxes at $40)",
			thread: "Buyer:\nHello,\nWe would like to order 40 boxes of ceramic mugs for our retail gift shop.\nCould you let us know your wholesale pricing and shipping details?\n\nSeller:\nHello,\nOur wholesale price is $40 per box for 40 boxes, shipping is $24, and payment is due on receipt.\nWe have all 40 boxes in stock and ready to ship.\n\nBuyer:\nThat works for us.\nPlease send over the invoice and we will settle it upon receipt.",
			req: offer.Request{
				Currency:             "USD",
				Quantity:             40,
				UnitPriceCents:       4000,
				RequestedPaymentDays: 0,
				RequestedDiscountBP:  0,
			},
		},
		{
			id:    "email-05",
			title: "Exam email 05 (testdata/emails/05.txt, porcelain planters, 25 boxes at $42, net 60 asked)",
			thread: "Buyer:\nHi,\nWe want to purchase 25 boxes of handmade porcelain planters at $42 per box.\nCan you extend payment terms of net 60 days so we have two months to pay?\n\nSeller:\nHi,\nWe have 25 boxes of porcelain planters at $42 each.\nHowever, we cannot do net 60 days as waiting two months creates too much unpaid balance risk.\nThe best we can offer is net 14 days, with $18 shipping.\n\nBuyer:\nFair enough, we understand.\nNet 14 days works for us on the 25 boxes at $42 each.\nPlease send the bill.",
			req: offer.Request{
				Currency:             "USD",
				Quantity:             25,
				UnitPriceCents:       4200,
				RequestedPaymentDays: 60,
				RequestedDiscountBP:  0,
			},
		},
		{
			id:    "email-12",
			title: "Exam email 12 (testdata/emails/12.txt, beeswax candles, 25 boxes at $30, 20% discount & net 60 asked)",
			thread: "Buyer:\n> Hi,\n> We are preparing an order for 25 boxes of scented beeswax candles.\n> Your wholesale catalog lists them at $30 per box.\n> We would like a 20% discount, free shipping, and net 60 payment terms.\n\nSeller:\n> Hi,\n> Yes, 25 boxes at $30 per box is available.\n> We can agree to a 20% discount, free shipping, and net 60 days payment terms.\n\nBuyer:\n> Thank you, 25 boxes at $30 with 20% off, free shipping, and net 60 days works for us.\n> Please send the PayPal draft invoice.",
			req: offer.Request{
				Currency:             "USD",
				Quantity:             25,
				UnitPriceCents:       3000,
				RequestedPaymentDays: 60,
				RequestedDiscountBP:  2000,
			},
		},
	}

	var report strings.Builder
	report.WriteString("Live Writer Samples Report (Step 4, Fix C)\n")
	report.WriteString("==========================================\n")
	fmt.Fprintf(&report, "Date: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&report, "Prompt version: %s\n", PromptVersion)
	report.WriteString("Model: gemini-3.5-flash-lite\n")
	report.WriteString("Rule: at most 8 live Gemini calls across all cases\n\n")

	totalCallsBefore := 0
	acceptedCount := 0
	fallbackCount := 0
	for i, tc := range cases {
		t.Logf("Running case %d: %s (%s)", i+1, tc.id, tc.title)
		callsStart := len(recorder.calls)

		set, err := offer.Build(tc.req, seller, offer.Config{})
		if err != nil {
			t.Fatalf("offer.Build for %s: %v", tc.id, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		draft := Write(ctx, Config{
			Client: recorder,
			Model:  "gemini-3.5-flash-lite",
		}, set)
		cancel()

		if draft.ByCode {
			fallbackCount++
		} else {
			acceptedCount++
		}

		callsThisCase := recorder.calls[callsStart:]

		report.WriteString(strings.Repeat("-", 70) + "\n")
		fmt.Fprintf(&report, "Case %d: %s\n", i+1, tc.title)
		fmt.Fprintf(&report, "Identifier: %s\n", tc.id)
		fmt.Fprintf(&report, "Offered choices: %d\n", len(set.Options))
		for optIdx, opt := range set.Options {
			fmt.Fprintf(&report, "  - Option %d (%s): %s\n    Reason: %s\n", optIdx+1, Label(opt.Kind), opt.Summary, opt.Reason)
		}
		if len(set.Omitted) > 0 {
			fmt.Fprintf(&report, "  - Omitted: %s\n", strings.Join(set.Omitted, "; "))
		}
		report.WriteString("\nThread text:\n")
		for _, line := range strings.Split(tc.thread, "\n") {
			report.WriteString("  | " + line + "\n")
		}
		report.WriteString("\nExecution Details:\n")
		fmt.Fprintf(&report, "  Attempts made: %d\n", draft.Attempts)
		fmt.Fprintf(&report, "  Live API calls in this case: %d\n", len(callsThisCase))
		fmt.Fprintf(&report, "  Tokens consumed: %d\n", draft.Tokens)
		fmt.Fprintf(&report, "  Used code template: %v\n", draft.ByCode)

		for attemptIdx, call := range callsThisCase {
			fmt.Fprintf(&report, "\n  [Attempt %d]\n", attemptIdx+1)
			if call.err != nil {
				fmt.Fprintf(&report, "    API Error: %v\n", call.err)
			} else {
				fmt.Fprintf(&report, "    Tokens: %d\n", call.resp.TotalTokens)
				fmt.Fprintf(&report, "    Raw Model Reply Text:\n")
				for _, line := range strings.Split(call.resp.Text, "\n") {
					fmt.Fprintf(&report, "      %s\n", line)
				}
				// Check validation problems on raw text if it was json
				var out struct {
					Reply string `json:"reply"`
				}
				if jsonErr := json.Unmarshal([]byte(call.resp.Text), &out); jsonErr != nil {
					fmt.Fprintf(&report, "    JSON Parse Error: %v\n", jsonErr)
				} else {
					probs := Validate(out.Reply, len(set.Options))
					if len(probs) > 0 {
						fmt.Fprintf(&report, "    Validation Problems:\n")
						for _, p := range probs {
							fmt.Fprintf(&report, "      - %s\n", p)
						}
					} else {
						fmt.Fprintf(&report, "    Validation: PASS (0 problems)\n")
					}
				}
			}
		}

		report.WriteString("\nFinal Accepted Reply:\n")
		report.WriteString("--------------------\n")
		for _, line := range strings.Split(draft.Text, "\n") {
			report.WriteString(line + "\n")
		}
		report.WriteString("--------------------\n\n")

		totalCallsBefore = len(recorder.calls)
		t.Logf("Case %s complete: %d calls, byCode=%v, tokens=%d", tc.id, len(callsThisCase), draft.ByCode, draft.Tokens)
	}

	report.WriteString("==========================================\n")
	fmt.Fprintf(&report, "Total live Gemini API calls across all cases: %d (limit: <= 8)\n", totalCallsBefore)
	fmt.Fprintf(&report, "Accepted from AI: %d / %d\n", acceptedCount, len(cases))
	fmt.Fprintf(&report, "Fell back to template: %d / %d\n", fallbackCount, len(cases))
	if totalCallsBefore > 8 {
		t.Errorf("exceeded live call budget: %d > 8", totalCallsBefore)
	}

	// Write report to margin-check/reports/step4-writer-samples-2.txt
	reportPaths := []string{
		"../../reports/step4-writer-samples-2.txt",
		"../reports/step4-writer-samples-2.txt",
		"reports/step4-writer-samples-2.txt",
	}
	written := false
	for _, rp := range reportPaths {
		dir := filepath.Dir(rp)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			if err := os.WriteFile(rp, []byte(report.String()), 0644); err == nil {
				t.Logf("Wrote report to %s", rp)
				written = true
				break
			}
		}
	}
	if !written {
		t.Errorf("failed to write step4-writer-samples-2.txt")
	}
}
