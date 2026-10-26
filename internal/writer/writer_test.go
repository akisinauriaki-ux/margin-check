package writer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"margincheck/internal/gemini"
	"margincheck/internal/offer"
	"margincheck/internal/profit"
)

type fakeGen struct {
	replies []string
	err     error
	seen    []gemini.Request
}

func (f *fakeGen) Generate(_ context.Context, req gemini.Request) (gemini.Response, error) {
	f.seen = append(f.seen, req)
	if f.err != nil {
		return gemini.Response{}, f.err
	}
	if len(f.replies) == 0 {
		return gemini.Response{}, errors.New("no more replies")
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return gemini.Response{Text: r, TotalTokens: 100, Source: "live"}, nil
}

func sampleSet(t *testing.T) offer.Set {
	t.Helper()
	seller := profit.Seller{UnitCostCents: 2300, ShippingCostCentsPerOrder: 600, ShippingCostCentsPerBox: 150,
		PayPalFeePercentBP: 349, PayPalFixedFeeCents: 49, YearlyCostOfMoneyBP: 1000, UnderlineMinGiveawayBP: 200, Currency: "USD"}
	s, err := offer.Build(offer.Request{Currency: "USD", Quantity: 20, UnitPriceCents: 4000, RequestedPaymentDays: 60}, seller, offer.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const good = `{"reply":"Thank you for thinking of us. Here is what we can do:\n\n{{OPTION_1}}\n{{OPTION_2}}\n{{OPTION_3}}\n\nWhat works best for you?"}`

func TestAGoodReplyIsFilledByCode(t *testing.T) {
	set := sampleSet(t)
	g := &fakeGen{replies: []string{good}}
	d := Write(context.Background(), Config{Client: g, Model: "m"}, set)
	if d.ByCode || d.Attempts != 1 {
		t.Fatalf("by code %v, attempts %d, problems %v", d.ByCode, d.Attempts, d.Problems)
	}
	for _, o := range set.Options {
		if !strings.Contains(d.Text, o.Summary) {
			t.Errorf("reply lacks %q:\n%s", o.Summary, d.Text)
		}
	}
	if strings.Contains(d.Text, "{{") {
		t.Errorf("an unfilled blank remains:\n%s", d.Text)
	}
}

func TestTheAISeesNoEmailAndNoNumbers(t *testing.T) {
	set := sampleSet(t)
	g := &fakeGen{replies: []string{good}}
	Write(context.Background(), Config{Client: g, Model: "m"}, set)
	if len(g.seen) != 1 {
		t.Fatalf("%d calls", len(g.seen))
	}
	sent := g.seen[0].SystemInstruction + g.seen[0].UserText
	stripped := blankRe.ReplaceAllString(sent, "")
	if strings.ContainsAny(stripped, "$€£%") {
		t.Errorf("the AI was shown money or percentages:\n%s", stripped)
	}
	for _, o := range set.Options {
		if strings.Contains(sent, o.Summary) {
			t.Errorf("the AI was shown the numbered summary %q", o.Summary)
		}
	}
}

func TestANumberWrittenByTheAIIsRejectedThenRetried(t *testing.T) {
	set := sampleSet(t)
	bad := `{"reply":"We can do {{OPTION_1}} or {{OPTION_2}} or {{OPTION_3}}, so 40 boxes for $35 is fine."}`
	g := &fakeGen{replies: []string{bad, good}}
	d := Write(context.Background(), Config{Client: g, Model: "m"}, set)
	if d.ByCode || d.Attempts != 2 {
		t.Fatalf("by code %v, attempts %d", d.ByCode, d.Attempts)
	}
	if !strings.Contains(g.seen[1].UserText, "digit") {
		t.Errorf("the retry was not told why: %q", g.seen[1].UserText)
	}
}

func TestTwoBadRepliesFallBackToTheTemplate(t *testing.T) {
	set := sampleSet(t)
	split := `{"reply":"Let's meet in the middle: {{OPTION_1}} {{OPTION_2}} {{OPTION_3}}"}`
	missing := `{"reply":"How about {{OPTION_1}}?"}`
	g := &fakeGen{replies: []string{split, missing}}
	d := Write(context.Background(), Config{Client: g, Model: "m"}, set)
	if !d.ByCode {
		t.Fatalf("want the code template after two bad replies; problems %v", d.Problems)
	}
	for _, o := range set.Options {
		if !strings.Contains(d.Text, o.Summary) {
			t.Errorf("template lacks %q", o.Summary)
		}
	}
}

func TestAnUnavailableModelFallsBackToTheTemplate(t *testing.T) {
	set := sampleSet(t)
	d := Write(context.Background(), Config{Client: &fakeGen{err: errors.New("503")}, Model: "m"}, set)
	if !d.ByCode || d.Text == "" {
		t.Fatalf("by code %v, text %q", d.ByCode, d.Text)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		text string
		want string // a word expected in a problem, or "" for no problem
	}{
		{"Hi,\n{{OPTION_1}} {{OPTION_2}}. What works for you?", ""},
		{"{{OPTION_1}} {{OPTION_2}}. What works for you?", "greeting"},
		{"Hi {{OPTION_1}}. What works for you?", "missing"},
		{"Hi {{OPTION_1}} {{OPTION_1}} {{OPTION_2}}", "more than once"},
		{"Hi {{OPTION_1}} {{OPTION_2}} {{OPTION_9}}", "unknown blank"},
		{"Only 5 left {{OPTION_1}} {{OPTION_2}}", "digit"},
		{"Ten percent off {{OPTION_1}} {{OPTION_2}} costs €", "currency"},
		{"We could split the difference {{OPTION_1}} {{OPTION_2}}", "split"},
		{"Take ten percent off {{OPTION_1}} {{OPTION_2}}", "number word"},
		{"Forty boxes then {{OPTION_1}} {{OPTION_2}}", "number word"},
		{"One more thing:\n{{OPTION_1}} {{OPTION_2}}", ""},
		{"", "empty"},
		{strings.Repeat("word ", 170) + "{{OPTION_1}} {{OPTION_2}}", "words"},
	}
	for _, c := range cases {
		probs := Validate(c.text, 2)
		if c.want == "" && len(probs) > 0 {
			t.Errorf("%q: unexpected problems %v", c.text, probs)
		}
		if c.want != "" && !strings.Contains(strings.Join(probs, "; "), c.want) {
			t.Errorf("%q: problems %v, want one mentioning %q", c.text, probs, c.want)
		}
	}
}

// A number is a number in any script: full-width digits, fractions and other digit sets must be rejected
// like 0-9, and so must every currency sign, not only $, € and £.
func TestValidateRejectsNumbersAndCurrencySignsInAnyScript(t *testing.T) {
	cases := []struct{ text, want string }{
		{"Only ２０ left {{OPTION_1}} {{OPTION_2}}", "digit"},
		{"Take ½ off {{OPTION_1}} {{OPTION_2}}", "digit"},
		{"Only ٥ left {{OPTION_1}} {{OPTION_2}}", "digit"},
		{"Squared² {{OPTION_1}} {{OPTION_2}}", "digit"},
		{"It costs ¥ {{OPTION_1}} {{OPTION_2}}", "currency"},
		{"It costs ₾ {{OPTION_1}} {{OPTION_2}}", "currency"},
		{"It costs ₹ {{OPTION_1}} {{OPTION_2}}", "currency"},
		{"Take ％ off {{OPTION_1}} {{OPTION_2}}", "percent"},
	}
	for _, c := range cases {
		probs := Validate(c.text, 2)
		if !strings.Contains(strings.Join(probs, "; "), c.want) {
			t.Errorf("%q: problems %v, want one mentioning %q", c.text, probs, c.want)
		}
	}
}

func TestValidateGreetingRule(t *testing.T) {
	// The reply must open with a greeting before the choices (risk 2):
	// code checks that the first non-empty line contains no choice.
	cases := []struct {
		text string
		want string
	}{
		{"{{OPTION_1}}\n{{OPTION_2}}\nWhat works best?", "the reply must open with a greeting before the choices"},
		{"\n\n{{OPTION_1}}\n{{OPTION_2}}\nWhat works best?", "the reply must open with a greeting before the choices"},
		{"Option 1 (Standard): 20 boxes.\nOption 2: 40 boxes.\nWhat works best?", "the reply must open with a greeting before the choices"},
		{"Hi {{OPTION_1}} {{OPTION_2}}. What works for you?", "the reply must open with a greeting before the choices"},
		{"Thank you for thinking of us. Here is what we can do:\n\n{{OPTION_1}}\n{{OPTION_2}}\n\nWhat works best for you?", ""},
		{"Hi,\n{{OPTION_1}}\n{{OPTION_2}}\nWhat works best for you?", ""},
	}
	for _, c := range cases {
		probs := Validate(c.text, 2)
		hasProb := false
		for _, p := range probs {
			if p == "the reply must open with a greeting before the choices" {
				hasProb = true
				break
			}
		}
		if c.want != "" && !hasProb {
			t.Errorf("%q: want problem %q, got %v", c.text, c.want, probs)
		}
		if c.want == "" && hasProb {
			t.Errorf("%q: unexpected greeting problem: %v", c.text, probs)
		}
	}
}

