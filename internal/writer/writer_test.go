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
		{"Hi there,\n{{OPTION_1}} {{OPTION_2}}. What works for you?", ""},
		{"{{OPTION_1}} {{OPTION_2}}. What works for you?", "greeting"},
		{"Hi {{OPTION_1}}. What works for you?", "missing"},
		{"Hi {{OPTION_1}} {{OPTION_1}} {{OPTION_2}}", "more than once"},
		{"Hi {{OPTION_1}} {{OPTION_2}} {{OPTION_9}}", "unknown blank"},
		{"Only 5 left {{OPTION_1}} {{OPTION_2}}", "digit"},
		{"Ten percent off {{OPTION_1}} {{OPTION_2}} costs €", "currency"},
		{"We could split the difference {{OPTION_1}} {{OPTION_2}}", "split"},
		{"Take ten percent off {{OPTION_1}} {{OPTION_2}}", "number word"},
		{"Forty boxes then {{OPTION_1}} {{OPTION_2}}", "number word"},
		{"One more thing:\n{{OPTION_1}} {{OPTION_2}}\nWhat works for you?", ""},
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
	// The text before the first {{OPTION_ blank, trimmed, must contain at least two words with letters.
	cases := []struct {
		text string
		want string
	}{
		// a one-line reply with greeting, blanks and question, which passes
		{"Thank you for thinking of us. {{OPTION_1}} {{OPTION_2}} What works best for you?", ""},
		// a reply that starts with a blank, which fails
		{"{{OPTION_1}} {{OPTION_2}} What works best for you?", "the reply must open with a greeting before the choices"},
		{"\n\n{{OPTION_1}}\n{{OPTION_2}}\nWhat works best?", "the reply must open with a greeting before the choices"},
		// a reply that starts with "Option", which fails
		{"Option 1: {{OPTION_1}} {{OPTION_2}} What works best?", "the reply must open with a greeting before the choices"},
		{"Option 1 (Standard): 20 boxes.\nOption 2: 40 boxes.\nWhat works best?", "the reply must open with a greeting before the choices"},
		{"Option {{OPTION_1}} {{OPTION_2}} What works best?", "the reply must open with a greeting before the choices"},
		// other greeting cases
		{"Hi {{OPTION_1}} {{OPTION_2}}. What works for you?", "the reply must open with a greeting before the choices"},
		{"Hi,\n{{OPTION_1}}\n{{OPTION_2}}\nWhat works best for you?", "the reply must open with a greeting before the choices"},
		{"Thank you for thinking of us. Here is what we can do:\n\n{{OPTION_1}}\n{{OPTION_2}}\n\nWhat works best for you?", ""},
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

func TestFillLayout(t *testing.T) {
	set := sampleSet(t)
	// a one-line AI reply becomes greeting, blank line, three choice lines, blank line, question.
	oneLine := "Thank you for reaching out today. {{OPTION_1}} {{OPTION_2}} {{OPTION_3}} What works best for you?"
	got := Fill(oneLine, set)
	want := "Thank you for reaching out today.\n\n" +
		"Option 1 (Standard): 20 boxes at $40.00 each, shipping $36.00 at cost, payment on receipt.\n" +
		"Option 2 (Quantity): 30 boxes at $40.00 each with free shipping, payment on receipt.\n" +
		"Option 3 (Early payment): 20 boxes at $40.00 each with 1.5% off, shipping $36.00 at cost, payment within 7 days.\n\n" +
		"What works best for you?"
	if got != want {
		t.Errorf("Fill layout mismatch:\nGOT:\n%q\n\nWANT:\n%q", got, want)
	}

	// Collapse 3 or more newlines into 2
	multiLine := "Thank you for reaching out today.\n\n\n\n{{OPTION_1}} {{OPTION_2}} {{OPTION_3}}\n\n\nWhat works best for you?"
	gotMulti := Fill(multiLine, set)
	if gotMulti != want {
		t.Errorf("Fill multi-newline collapse mismatch:\nGOT:\n%q\n\nWANT:\n%q", gotMulti, want)
	}
}

func TestValidateRejectsArgonM5Bypasses(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{
			name: "Bypass_0_Reordered_Blanks",
			text: "Thank you for reaching out. {{OPTION_2}} {{OPTION_1}} {{OPTION_3}} How does that sound?",
		},
		{
			name: "Bypass_1_One_Month_And_One_Box_Free",
			text: "Thank you for reaching out. {{OPTION_1}} {{OPTION_2}} {{OPTION_3}} How about one month to pay, or one box free?",
		},
		{
			name: "Bypass_2_Ordinal_And_Fraction_Words",
			text: "Thank you for reaching out. {{OPTION_1}} {{OPTION_2}} {{OPTION_3}} What if we take a quarter off the order, or a fifth off on a third shipment?",
		},
		{
			name: "Bypass_3_Concession_Words_Extra_And_Off",
			text: "Thank you for reaching out. {{OPTION_1}} {{OPTION_2}} {{OPTION_3}} What if we include X extra boxes or knock V off each box?",
		},
		{
			name: "Bypass_4_Currency_Words_And_Codes",
			text: "Thank you for reaching out. {{OPTION_1}} {{OPTION_2}} {{OPTION_3}} What if we knock a few bucks or pounds sterling off in USD or GBP?",
		},
		{
			name: "Bypass_5_Meet_In_The_Middle_Variants",
			text: "Thank you for reaching out. {{OPTION_1}} {{OPTION_2}} {{OPTION_3}} How about we meet you in the middle and split the gap?",
		},
		{
			name: "Bypass_6_Concession_Waive_And_No_Closing_Question",
			text: "Thank you for reaching out. {{OPTION_1}} We will also waive all shipping fees for you. {{OPTION_2}} {{OPTION_3}} Let us know.",
		},
	}
	for _, tc := range cases {
		probs := Validate(tc.text, 3)
		if len(probs) == 0 {
			t.Errorf("%s was not rejected by Validate", tc.name)
		}
	}
}

func TestValidateAcceptsAllRepliesFromSamples2(t *testing.T) {
	accepted := []struct {
		name string
		n    int
		raw  string
	}{
		{
			name: "Case 1 (sample)",
			n:    3,
			raw:  "Thank you so much for reaching out to us today. {{OPTION_1}} {{OPTION_2}} {{OPTION_3}} What works best for you?",
		},
		{
			name: "Case 2 (email-01)",
			n:    2,
			raw:  "Thank you so much for reaching out to us today. {{OPTION_1}} {{OPTION_2}} What would you like to do next?",
		},
		{
			name: "Case 3 (email-05)",
			n:    3,
			raw:  "Thank you so much for reaching out to us today. {{OPTION_1}} {{OPTION_2}} {{OPTION_3}} What works best for your current situation?",
		},
		{
			name: "Case 4 (email-12)",
			n:    3,
			raw:  "Thank you so much for reaching out to us today. {{OPTION_1}} {{OPTION_2}} {{OPTION_3}} What works best for your needs?",
		},
	}
	for _, tc := range accepted {
		probs := Validate(tc.raw, tc.n)
		if len(probs) > 0 {
			t.Errorf("%s failed validation: %v", tc.name, probs)
		}
	}
}

func TestValidateM5RulesDetail(t *testing.T) {
	cases := []struct {
		name string
		text string
		n    int
		want string
	}{
		// "one" or "a" followed by unit or time word
		{"one month", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about one month? What works?", 2, "terms"},
		{"one box", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about one box? What works?", 2, "terms"},
		{"a month", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about a month? What works?", 2, "terms"},
		{"a box", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about a box? What works?", 2, "terms"},
		{"one unit", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about one unit? What works?", 2, "terms"},
		{"a unit", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about a unit? What works?", 2, "terms"},
		{"one week", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about one week? What works?", 2, "terms"},
		{"a week", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about a week? What works?", 2, "terms"},
		{"one day", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about one day? What works?", 2, "terms"},
		{"a day", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about a day? What works?", 2, "terms"},
		{"one year", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about one year? What works?", 2, "terms"},
		{"a year", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about a year? What works?", 2, "terms"},
		{"one percent", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about one percent? What works?", 2, "terms"},
		{"a percent", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow about a percent? What works?", 2, "terms"},

		// fraction and ordinal words: half, quarter, third, fourth, fifth, tenth, double, twice, triple
		{"quarter", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nTake a quarter off. What works?", 2, "fraction or ordinal"},
		{"third", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nOn a third shipment. What works?", 2, "fraction or ordinal"},
		{"fourth", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nOn a fourth order. What works?", 2, "fraction or ordinal"},
		{"fifth", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA fifth off. What works?", 2, "fraction or ordinal"},
		{"tenth", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA tenth off. What works?", 2, "fraction or ordinal"},
		{"double", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nWe double it. What works?", 2, "fraction or ordinal"},
		{"twice", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nPay twice. What works?", 2, "fraction or ordinal"},
		{"triple", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nTriple the amount. What works?", 2, "fraction or ordinal"},

		// concession words: waive, free, discount, extra, bonus, gift, refund, rebate, cheaper, "off" as a whole word
		{"waive", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nWe waive shipping. What works?", 2, "concession"},
		{"free", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nFree shipping for you. What works?", 2, "concession"},
		{"discount", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA discount for you. What works?", 2, "concession"},
		{"extra", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nSome extra items. What works?", 2, "concession"},
		{"bonus", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA bonus item. What works?", 2, "concession"},
		{"gift", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA gift from us. What works?", 2, "concession"},
		{"refund", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA refund later. What works?", 2, "concession"},
		{"rebate", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA rebate later. What works?", 2, "concession"},
		{"cheaper", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nIt is cheaper. What works?", 2, "concession"},
		{"off", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nKnock some off. What works?", 2, "concession"},

		// currency words and codes: usd, eur, gbp, pound, pounds, sterling, buck, bucks, quid
		{"usd", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nIn USD only. What works?", 2, "currency word"},
		{"eur", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nIn EUR only. What works?", 2, "currency word"},
		{"gbp", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nIn GBP only. What works?", 2, "currency word"},
		{"pound", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA pound each. What works?", 2, "currency word"},
		{"pounds", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nFew pounds each. What works?", 2, "currency word"},
		{"sterling", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nIn sterling. What works?", 2, "currency word"},
		{"buck", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA buck each. What works?", 2, "currency word"},
		{"bucks", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nFew bucks each. What works?", 2, "currency word"},
		{"quid", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA quid each. What works?", 2, "currency word"},

		// more "meet in the middle" phrases: "in the middle", "middle ground", "split the", "meet you", compromise
		{"in the middle", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nLet us be in the middle. What works?", 2, "splitting the difference"},
		{"middle ground", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nFind middle ground. What works?", 2, "splitting the difference"},
		{"split the", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nSplit the cost. What works?", 2, "splitting the difference"},
		{"meet you", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nWe can meet you on this. What works?", 2, "splitting the difference"},
		{"compromise", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nA fair compromise. What works?", 2, "splitting the difference"},

		// closing question check
		{"no question", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nLet us know.", 2, "question containing 'how' or 'what'"},
		{"question without how or what", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nAre you interested?", 2, "question containing 'how' or 'what'"},
		{"how question without mark", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow does that sound.", 2, "question containing 'how' or 'what'"},
		{"valid how question", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nHow does that sound?", 2, ""},
		{"valid what question", "Hi there,\n{{OPTION_1}} {{OPTION_2}}\nWhat works best for you?", 2, ""},
	}
	for _, tc := range cases {
		probs := Validate(tc.text, tc.n)
		joined := strings.Join(probs, "; ")
		if tc.want == "" && len(probs) > 0 {
			t.Errorf("%s: unexpected problems: %v", tc.name, probs)
		}
		if tc.want != "" && !strings.Contains(joined, tc.want) {
			t.Errorf("%s: problems %v, want one mentioning %q", tc.name, probs, tc.want)
		}
	}
}


