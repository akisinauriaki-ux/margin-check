package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"seconds per deal: avg",
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
