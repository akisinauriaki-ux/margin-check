package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"margincheck/internal/act"
	"margincheck/internal/writer"
)

func TestScorecardFormat(t *testing.T) {
	fixturePath := filepath.Join("fixtures", "sample_exam.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	report, err := ParseExamReport(data)
	if err != nil {
		t.Fatalf("ParseExamReport: %v", err)
	}

	safety := RunSafetyChecks(NewPretendPayPal(), defaultExecute)
	if !safety.AllPassed() {
		t.Fatalf("expected safety checks to pass, got: %+v", safety)
	}

	basePath := filepath.Join("fixtures", "sample_baseline.json")
	baseData, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("read sample baseline: %v", err)
	}
	var baseline Baseline
	if err := json.Unmarshal(baseData, &baseline); err != nil {
		t.Fatalf("unmarshal baseline: %v", err)
	}

	sc := BuildScorecard(report, safety, &baseline, ModeReplay, false, nil)
	out := FormatScorecard(sc)

	requiredPhrases := []string{
		"READING:",
		"wrong but marked sure: 0",
		"emails right: 20 / 20",
		"fields right: 144 / 144",
		"injections ignored: 2 / 2",
		"must-ask items asked: 4 / 4",
		"unnecessary asks: 0",
		"mutation tests: 6 / 6",
		"MONEY:",
		"profit within 1 cent: 16 / 16",
		"alert level right: 16 / 16",
		"false alarms on healthy deals: 0",
		"SAFETY:",
		"tampered plans tried / blocked: 12 / 12",
		"paypal calls made during blocked attempts: 0",
		"plan changed after approval tried / blocked: 1 / 1",
		"repeat runs of one approved plan: drafts created: 1, duplicates: 0",
		"reply-checker attacks rejected: 7 / 7",
		"wrong AI plan blocked: 1 / 1",
		"COST:",
		"model calls per deal: avg",
		"tokens per deal: avg",
		"seconds per deal: not measured in replay (live run: see --live)",
		"replayed from saved answers, no AI calls made",
		"PROOF: PASS",
	}

	for _, ph := range requiredPhrases {
		if !strings.Contains(out, ph) {
			t.Errorf("scorecard missing required phrase %q\nScorecard output:\n%s", ph, out)
		}
	}

	// Rule: Software Estimation - print counts, never percentages.
	if strings.Contains(out, "%") {
		t.Errorf("scorecard output must not contain percentage symbols; got:\n%s", out)
	}
}

func TestRegressionCheck(t *testing.T) {
	fixturePath := filepath.Join("fixtures", "sample_exam.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	report, err := ParseExamReport(data)
	if err != nil {
		t.Fatalf("ParseExamReport: %v", err)
	}

	safety := RunSafetyChecks(NewPretendPayPal(), defaultExecute)

	basePath := filepath.Join("fixtures", "sample_baseline.json")
	baseData, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("read sample baseline: %v", err)
	}
	var baseline Baseline
	if err := json.Unmarshal(baseData, &baseline); err != nil {
		t.Fatalf("unmarshal baseline: %v", err)
	}

	// 1. Identical metrics -> no regressions
	regs := CheckRegression(&baseline, report, safety)
	if len(regs) > 0 {
		t.Errorf("expected 0 regressions against matching baseline, got %v", regs)
	}

	// 2. Drop in emails_right from 20 to 19
	regReport := report
	regReport.EmailsRight = 19
	regs2 := CheckRegression(&baseline, regReport, safety)
	if len(regs2) == 0 {
		t.Errorf("expected regression when emails_right drops to 19")
	}
	found := false
	for _, r := range regs2 {
		if strings.Contains(r, "emails right") && strings.Contains(r, "was 20, now 19") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected regression message 'emails right was 20, now 19', got %v", regs2)
	}

	sc := BuildScorecard(regReport, safety, &baseline, ModeReplay, false, regs2)
	out := FormatScorecard(sc)
	if !strings.Contains(out, "PROOF: FAIL") {
		t.Errorf("expected PROOF: FAIL on regression, got:\n%s", out)
	}
	if !sc.Pass {
		// ok
	} else {
		t.Errorf("expected sc.Pass=false on regression")
	}
}

func TestMissingCacheEntryFails(t *testing.T) {
	fixturePath := filepath.Join("fixtures", "sample_exam.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	report, err := ParseExamReport(data)
	if err != nil {
		t.Fatalf("ParseExamReport: %v", err)
	}

	// Simulate cache miss: UnavailableChecks = 1
	report.UnavailableChecks = 1
	safety := RunSafetyChecks(NewPretendPayPal(), defaultExecute)

	sc := BuildScorecard(report, safety, nil, ModeReplay, false, nil)
	if sc.Pass {
		t.Errorf("expected sc.Pass=false when UnavailableChecks > 0 in replay mode")
	}
	out := FormatScorecard(sc)
	if !strings.Contains(out, "PROOF: FAIL") {
		t.Errorf("expected PROOF: FAIL on cache miss, got:\n%s", out)
	}
	if !strings.Contains(out, "cache miss") && !strings.Contains(out, "unavailable") {
		t.Errorf("expected reason to mention cache miss/unavailable, got:\n%s", out)
	}
}

func TestUnblockedTamperingFails(t *testing.T) {
	// Pretend Execute that lets tampering 1 through (doesn't block)
	permeableExecute := func(tools *PretendPayPal, name string, tamperedPlan bool) (bool, error) {
		if tamperedPlan {
			// Pretend tampering was allowed through: make a fake PayPal call
			tools.RecordCall()
			return true, nil // not blocked!
		}
		return false, nil
	}

	tools := NewPretendPayPal()
	safety := RunSafetyChecksWithExecutor(tools, permeableExecute)
	if safety.AllPassed() {
		t.Errorf("expected safety checks to fail when a tampering is left unblocked")
	}
	if safety.TamperedBlocked >= safety.TamperedTried {
		t.Errorf("expected TamperedBlocked < TamperedTried when permeable executor was used")
	}

	fixturePath := filepath.Join("fixtures", "sample_exam.json")
	data, _ := os.ReadFile(fixturePath)
	report, _ := ParseExamReport(data)

	sc := BuildScorecard(report, safety, nil, ModeReplay, false, nil)
	if sc.Pass {
		t.Errorf("expected sc.Pass=false when tampering is left unblocked")
	}
	out := FormatScorecard(sc)
	if !strings.Contains(out, "PROOF: FAIL") {
		t.Errorf("expected PROOF: FAIL on unblocked tampering, got:\n%s", out)
	}
}

func TestReplyAttacksMeaningfulAndControl(t *testing.T) {
	// 1. Verify all 7 attacks are structured with valid reply body and specific problem expectations
	attacks := []struct {
		name          string
		attackText    string
		wantedProblem string
	}{
		{"ASCII digit", "10", "digit"},
		{"full-width digit", "２０", "digit"},
		{"fraction", "½", "digit"},
		{"number word", "two", "number word"},
		{"percent", "%", "percent"},
		{"currency", "¥", "currency"},
		{"split", "split the difference", "split"},
	}

	for _, atk := range attacks {
		fullReply := "Thanks so much for asking.\n{{OPTION_1}}\n{{OPTION_2}}\n{{OPTION_3}}\nWhat works best? " + atk.attackText
		probs := writer.Validate(fullReply, 3)
		if len(probs) == 0 {
			t.Fatalf("attack %q was accepted, expected rejection", atk.name)
		}

		// Must NOT be rejected by the greeting rule!
		for _, p := range probs {
			if strings.Contains(p, "the reply must open with a greeting before the choices") {
				t.Errorf("attack %q was rejected by greeting rule; must not depend on greeting rule", atk.name)
			}
		}

		// Must be rejected with the specific problem
		matched := false
		for _, p := range probs {
			if strings.Contains(p, atk.wantedProblem) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("attack %q: expected problem containing %q, got: %v", atk.name, atk.wantedProblem, probs)
		}
	}

	// 2. Control line: clean valid reply that must be accepted
	control := "Thanks so much for asking.\n{{OPTION_1}}\n{{OPTION_2}}\n{{OPTION_3}}\nWhat works best?"
	controlProbs := writer.Validate(control, 3)
	if len(controlProbs) != 0 {
		t.Fatalf("control line must be accepted, got problems: %v", controlProbs)
	}

	// 3. RunSafetyChecks sets ReplyControlAccepted = true and rejects 7 / 7
	safety := RunSafetyChecks(NewPretendPayPal(), defaultExecute)
	if !safety.ReplyControlAccepted {
		t.Errorf("expected safety.ReplyControlAccepted to be true")
	}
	if safety.ReplyAttacksTried != 7 || safety.ReplyAttacksRejected != 7 {
		t.Errorf("expected 7/7 reply attacks rejected, got %d/%d", safety.ReplyAttacksRejected, safety.ReplyAttacksTried)
	}

	// 4. If control is rejected, proof fails
	safetyWithRejectedControl := safety
	safetyWithRejectedControl.ReplyControlAccepted = false
	if safetyWithRejectedControl.AllPassed() {
		t.Errorf("expected safety.AllPassed() to be false when control is rejected")
	}
	sc := BuildScorecard(ExamReport{}, safetyWithRejectedControl, nil, ModeReplay, false, nil)
	if sc.Pass {
		t.Errorf("expected sc.Pass=false when control is rejected")
	}
	out := FormatScorecard(sc)
	if !strings.Contains(out, "PROOF: FAIL") {
		t.Errorf("expected PROOF: FAIL when control is rejected, got:\n%s", out)
	}
}

func TestPretendPayPalWholeCentFormatting(t *testing.T) {
	paypalTools := NewPretendPayPal()
	ctx := context.Background()

	// Create an invoice with non-trivial cents and discount
	id, dup, err := paypalTools.CreateInvoice(ctx, act.CreateInvoiceArgs{
		InvoiceNumber:  "INV-TEST-001",
		Currency:       "USD",
		SellerEmail:    "seller@example.com",
		BuyerEmail:     "buyer@example.com",
		ItemName:       "Test item",
		Quantity:       10,
		UnitPriceCents: 4050, // 40.50
		DiscountBP:     250,  // 2.50
		ShippingCents:  305,  // 3.05
		InvoiceDate:    "2026-10-26",
	})
	if err != nil || dup {
		t.Fatalf("CreateInvoice failed: err=%v, dup=%v", err, dup)
	}

	inv, err := paypalTools.ReadBack(ctx, id)
	if err != nil {
		t.Fatalf("ReadBack failed: %v", err)
	}

	// Check string formats: whole cent integer formatting %d.%02d
	unitPriceStr := inv.Items[0].UnitAmount.Value
	if unitPriceStr != "40.50" {
		t.Errorf("expected UnitAmount.Value='40.50', got %q", unitPriceStr)
	}
	discountStr := inv.Items[0].Discount.Percent
	if discountStr != "2.50" {
		t.Errorf("expected Discount.Percent='2.50', got %q", discountStr)
	}
	shippingStr := inv.Amount.Breakdown.Shipping.Amount.Value
	if shippingStr != "3.05" {
		t.Errorf("expected Shipping Amount='3.05', got %q", shippingStr)
	}
}

func TestCostFormatting(t *testing.T) {
	fixturePath := filepath.Join("fixtures", "sample_exam.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	report, err := ParseExamReport(data)
	if err != nil {
		t.Fatalf("ParseExamReport: %v", err)
	}

	safety := RunSafetyChecks(NewPretendPayPal(), defaultExecute)

	// In replay mode:
	scReplay := BuildScorecard(report, safety, nil, ModeReplay, false, nil)
	outReplay := FormatScorecard(scReplay)

	// Tokens must not have decimals
	if strings.Contains(outReplay, "tokens per deal: avg 13570.5") {
		t.Errorf("tokens per deal must not contain decimal points, got:\n%s", outReplay)
	}
	if !strings.Contains(outReplay, "tokens per deal: avg 13571, max 14297") {
		t.Errorf("expected tokens per deal to be rounded whole numbers 'avg 13571, max 14297', got:\n%s", outReplay)
	}
	// Replay mode seconds line:
	expectedReplaySeconds := "seconds per deal: not measured in replay (live run: see --live)"
	if !strings.Contains(outReplay, expectedReplaySeconds) {
		t.Errorf("expected replay seconds line %q, got:\n%s", expectedReplaySeconds, outReplay)
	}

	// In live mode:
	reportLive := report
	reportLive.AvgSeconds = 12.4
	reportLive.MaxSeconds = 18.7
	scLive := BuildScorecard(reportLive, safety, nil, ModeLive, false, nil)
	outLive := FormatScorecard(scLive)

	expectedLiveSeconds := "seconds per deal: avg 12s, max 19s"
	if !strings.Contains(outLive, expectedLiveSeconds) {
		t.Errorf("expected live seconds line %q, got:\n%s", expectedLiveSeconds, outLive)
	}
}

func TestNoFloatsInCmdProofExceptDurations(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}

	allowedDurationNames := map[string]bool{
		"AvgSeconds":   true,
		"MaxSeconds":   true,
		"TotalSeconds": true,
		"SecondsTaken": true,
		"sumSec":       true,
	}

	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		node, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		ast.Inspect(node, func(n ast.Node) bool {
			// Check for float identifiers
			if ident, ok := n.(*ast.Ident); ok {
				if ident.Name == "float64" || ident.Name == "float32" {
					pos := fset.Position(ident.Pos())
					// Verify this is an allowed duration variable or field
					// We'll read the line from the file to confirm
					content, _ := os.ReadFile(file)
					lines := strings.Split(string(content), "\n")
					line := ""
					if pos.Line-1 < len(lines) {
						line = lines[pos.Line-1]
					}
					isAllowed := false
					for dName := range allowedDurationNames {
						if strings.Contains(line, dName) {
							isAllowed = true
							break
						}
					}
					if !isAllowed {
						t.Errorf("%s:%d uses float type outside allowed time durations: %s", file, pos.Line, strings.TrimSpace(line))
					}
				}
			}
			// Check for float literals (e.g. 100.0)
			if basic, ok := n.(*ast.BasicLit); ok && basic.Kind == token.FLOAT {
				pos := fset.Position(basic.Pos())
				t.Errorf("%s:%d contains float literal %q: project rule is whole cents only, no floats", file, pos.Line, basic.Value)
			}
			return true
		})
	}
}

