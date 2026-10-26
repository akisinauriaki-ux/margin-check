// Command exam runs the 20-email reader evaluation, single-model ranking,
// two-model pair evaluation, injection checks, term accuracy (M1), and
// counterfactual mutation tests (--mutations).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"margincheck/internal/gemini"
	"margincheck/internal/reader"
)

// AnswerKeyFile matches testdata/answers.json.
type AnswerKeyFile struct {
	Emails map[string]AnswerEmail `json:"emails"`
}

// AnswerEmail holds the expected deal, accepted_option, must_ask, and terms for one email.
type AnswerEmail struct {
	SourceRow      int             `json:"source_row"`
	HasInjection   bool            `json:"has_injection"`
	Deal           map[string]any  `json:"deal"`
	AcceptedOption *string         `json:"accepted_option"`
	MustAsk        []string        `json:"must_ask"`
	Terms          []AnswerKeyTerm `json:"terms"`
}

// AnswerKeyTerm is a single expected term in answers.json.
type AnswerKeyTerm struct {
	Name     string `json:"name"`
	Value    any    `json:"value"`
	Currency string `json:"currency,omitempty"`
	By       string `json:"by"`
	Status   string `json:"status"`
}

// MutationsFile matches testdata/mutations.json.
type MutationsFile struct {
	Mutations []MutationSpec `json:"mutations"`
}

// MutationSpec describes one counterfactual word/number edit on an email.
type MutationSpec struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Description string `json:"description"`
	Find        string `json:"find"`
	Replace     string `json:"replace"`
	Field       string `json:"field"`
	From        any    `json:"from"`
	To          any    `json:"to"`
}

// FieldOutcome classifies how a single scored field compared to the answer key.
type FieldOutcome string

const (
	OutcomeRight          FieldOutcome = "right"
	OutcomeWrongButSure   FieldOutcome = "wrong_but_sure"
	OutcomeUnnecessaryAsk FieldOutcome = "unnecessary_ask"
)

// FieldScore records the evaluation of one scored field on one email.
type FieldScore struct {
	Field          string       `json:"field"`
	Expected       string       `json:"expected"`
	Actual         string       `json:"actual"`
	Sure           bool         `json:"sure"`
	Reason         string       `json:"reason,omitempty"`
	Outcome        FieldOutcome `json:"outcome"`
	BothAgreed     bool         `json:"both_agreed"`
	BothWrongAgreed bool        `json:"both_wrong_agreed"`
}

// EmailScore records the evaluation of one email in the exam.
type EmailScore struct {
	EmailID            string       `json:"email_id"`
	HasInjection       bool         `json:"has_injection"`
	InjectionIgnored   bool         `json:"injection_ignored"`
	EmailWrong         bool         `json:"email_wrong"`
	WrongReasons       []string     `json:"wrong_reasons,omitempty"`
	ScoredFields       int          `json:"scored_fields"`
	RightFields        int          `json:"right_fields"`
	WrongButSure       int          `json:"wrong_but_sure"`
	BothAgreedWrong    int          `json:"both_agreed_wrong"`
	UnnecessaryAsks    int          `json:"unnecessary_asks"`
	MustAskTotal       int          `json:"must_ask_total"`
	MustAskAsked       int          `json:"must_ask_asked"`
	MustAskMissed      int          `json:"must_ask_missed"`
	FieldScores        []FieldScore `json:"field_scores"`
	TermsExpected      int          `json:"terms_expected"`
	TermsMatched       int          `json:"terms_matched"`
	TermsExtra         int          `json:"terms_extra"`
	CallsUsed          int          `json:"calls_used"`
	TokensUsed         int          `json:"tokens_used"`
	SecondsTaken       float64      `json:"seconds_taken"`
	Mode               string       `json:"mode"`
}

// TermStatusBreakdown tracks matched vs expected terms for one status ("requested", "offered", "agreed").
type TermStatusBreakdown struct {
	Expected int     `json:"expected"`
	Matched  int     `json:"matched"`
	Share    float64 `json:"share"`
}

// MutationScore records the outcome of one counterfactual mutation test.
type MutationScore struct {
	ID                  string   `json:"id"`
	Email               string   `json:"email"`
	Field               string   `json:"field"`
	ExpectedTo          string   `json:"expected_to"`
	ActualTo            string   `json:"actual_to"`
	TargetChangedRight  bool     `json:"target_changed_right"`
	OtherFieldsUnchanged bool    `json:"other_fields_unchanged"`
	ChangedOtherFields  []string `json:"changed_other_fields,omitempty"`
	Passed              bool     `json:"passed"`
	CallsUsed           int      `json:"calls_used"`
	TokensUsed          int      `json:"tokens_used"`
	SecondsTaken        float64  `json:"seconds_taken"`
}

// ExamSummary is the top-level JSON report saved to margin-check/reports/.
type ExamSummary struct {
	Date                   string                         `json:"date"`
	Dir                    string                         `json:"dir"`
	InstructionVersion     string                         `json:"instruction_version"`
	Models                 []string                       `json:"models"`
	SingleMode             bool                           `json:"single_mode"`
	EmailsTotal            int                            `json:"emails_total"`
	EmailsRight            int                            `json:"emails_right"`
	EmailsWrong            int                            `json:"emails_wrong"`
	ScoredFieldsTotal      int                            `json:"scored_fields_total"`
	ScoredFieldsRight      int                            `json:"scored_fields_right"`
	FieldAccuracyPct       float64                        `json:"field_accuracy_pct"`
	WrongButSureTotal      int                            `json:"wrong_but_sure_total"`
	BothAgreedWrongTotal   int                            `json:"both_agreed_wrong_total"`
	UnnecessaryAsksTotal   int                            `json:"unnecessary_asks_total"`
	MustAskTotal           int                            `json:"must_ask_total"`
	MustAskAsked           int                            `json:"must_ask_asked"`
	MustAskMissed          int                            `json:"must_ask_missed"`
	InjectionsTotal        int                            `json:"injections_total"`
	InjectionsIgnored      int                            `json:"injections_ignored"`
	TermsExpectedTotal     int                            `json:"terms_expected_total"`
	TermsMatchedTotal      int                            `json:"terms_matched_total"`
	TermsShareOverall      float64                        `json:"terms_share_overall"`
	TermsByStatus          map[string]TermStatusBreakdown `json:"terms_by_status"`
	TermsExtraTotal        int                            `json:"terms_extra_total"`
	MutationsTotal         int                            `json:"mutations_total"`
	MutationsPassed        int                            `json:"mutations_passed"`
	MutationScores         []MutationScore                `json:"mutation_scores,omitempty"`
	TotalCalls             int                            `json:"total_calls"`
	TotalTokens            int                            `json:"total_tokens"`
	TotalSeconds           float64                        `json:"total_seconds"`
	Kill2Pass              bool                           `json:"kill2_pass"`
	EmailScores            []EmailScore                   `json:"email_scores"`
}

// callCapGenerator wraps a reader.Generator to enforce --max-calls across an entire exam run.
type callCapGenerator struct {
	inner     reader.Generator
	maxCalls  int
	totalCalls int
	liveCalls  int
}

var errMaxCallsReached = errors.New("exam: --max-calls limit reached")

func (g *callCapGenerator) Generate(ctx context.Context, req gemini.Request) (gemini.Response, error) {
	if g.maxCalls > 0 && g.totalCalls >= g.maxCalls {
		return gemini.Response{}, errMaxCallsReached
	}
	g.totalCalls++
	resp, err := g.inner.Generate(ctx, req)
	if err == nil && !resp.Replayed {
		g.liveCalls++
	}
	return resp, err
}

func main() {
	var (
		dirFlag       = flag.String("dir", "testdata", "Folder with emails/*.txt, answers.json, and seller.json")
		modelsFlag    = flag.String("models", "", "Comma-separated model name(s), e.g. m1,m2 or m1 with --single")
		singleFlag    = flag.Bool("single", false, "Run single-model mode")
		emailsFlag    = flag.String("emails", "", "Optional comma-separated list of email IDs (e.g. 01,05)")
		mutationsFlag = flag.Bool("mutations", false, "Run counterfactual mutations from mutations.json")
		noCacheFlag   = flag.Bool("no-cache", false, "Bypass response cache")
		maxCallsFlag  = flag.Int("max-calls", 0, "Maximum model calls allowed for this run (0 = default)")
		promptFlag    = flag.String("prompt", "reader-v1", "Prompt version name in prompts/<name>.md or path")
	)
	flag.Parse()

	models := parseCSV(*modelsFlag)
	if len(models) == 0 {
		fmt.Fprintln(os.Stderr, "error: --models is required")
		os.Exit(2)
	}
	if *singleFlag && len(models) != 1 {
		fmt.Fprintln(os.Stderr, "error: --single requires exactly 1 model in --models")
		os.Exit(2)
	}
	if !*singleFlag && len(models) != 2 {
		fmt.Fprintln(os.Stderr, "error: two-model mode requires 2 different models in --models (or pass --single)")
		os.Exit(2)
	}
	if !*singleFlag && models[0] == models[1] {
		fmt.Fprintln(os.Stderr, "error: two-model mode requires two DIFFERENT models")
		os.Exit(2)
	}

	apiKey, err := loadKeyFromEnvOrParent()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	gemClient, err := gemini.NewClient(apiKey, filepath.Join(".cache", "gemini"), *noCacheFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	promptPath := resolvePromptPath(*promptFlag)
	promptVer := strings.TrimSuffix(filepath.Base(promptPath), filepath.Ext(promptPath))

	capGen := &callCapGenerator{
		inner:    gemClient,
		maxCalls: *maxCallsFlag,
	}

	modelA := models[0]
	modelB := ""
	if len(models) > 1 {
		modelB = models[1]
	}

	r, err := reader.New(reader.Config{
		Client:        capGen,
		PromptPath:    promptPath,
		PromptVersion: promptVer,
		ModelA:        modelA,
		ModelB:        modelB,
		Single:        *singleFlag,
		TraceDir:      filepath.Join("reports", "traces"),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error initializing reader: %v\n", err)
		os.Exit(1)
	}

	emailFilter := parseCSV(*emailsFlag)
	summary, err := runExam(context.Background(), r, *dirFlag, promptVer, models, *singleFlag, emailFilter, *mutationsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "exam stopped: %v\n", err)
		os.Exit(1)
	}

	txtReport := formatTextReport(summary)
	fmt.Print(txtReport)

	if err := saveReports(summary, txtReport); err != nil {
		fmt.Fprintf(os.Stderr, "error saving reports: %v\n", err)
		os.Exit(1)
	}
}

func loadKeyFromEnvOrParent() (string, error) {
	candidates := []string{
		".env",
		filepath.Join("..", ".env"),
		filepath.Join("..", "..", ".env"),
	}
	for _, p := range candidates {
		if k, err := gemini.LoadAPIKey(p); err == nil && k != "" {
			return k, nil
		}
	}
	return gemini.LoadAPIKey("")
}

func resolvePromptPath(promptArg string) string {
	if _, err := os.Stat(promptArg); err == nil {
		return promptArg
	}
	p := filepath.Join("prompts", promptArg+".md")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return promptArg
}

func parseCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func runExam(
	ctx context.Context,
	r *reader.Reader,
	dir string,
	promptVer string,
	models []string,
	single bool,
	emailFilter []string,
	runMutations bool,
) (ExamSummary, error) {
	ansBytes, err := os.ReadFile(filepath.Join(dir, "answers.json"))
	if err != nil {
		return ExamSummary{}, fmt.Errorf("read answers.json: %w", err)
	}
	var ansKey AnswerKeyFile
	if err := json.Unmarshal(ansBytes, &ansKey); err != nil {
		return ExamSummary{}, fmt.Errorf("parse answers.json: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "seller.json")); err != nil {
		return ExamSummary{}, fmt.Errorf("missing seller.json in %s: %w", dir, err)
	}

	var emailIDs []string
	if len(emailFilter) > 0 {
		emailIDs = emailFilter
	} else {
		for id := range ansKey.Emails {
			emailIDs = append(emailIDs, id)
		}
		sort.Strings(emailIDs)
	}

	summary := ExamSummary{
		Date:               time.Now().UTC().Format("2006-01-02"),
		Dir:                dir,
		InstructionVersion: promptVer,
		Models:             models,
		SingleMode:         single,
		TermsByStatus: map[string]TermStatusBreakdown{
			"requested": {},
			"offered":   {},
			"agreed":    {},
		},
	}

	origReadings := make(map[string]reader.CheckResult)

	for _, id := range emailIDs {
		keyEmail, ok := ansKey.Emails[id]
		if !ok {
			return summary, fmt.Errorf("email ID %q not found in answers.json", id)
		}
		emailPath := filepath.Join(dir, "emails", id+".txt")
		rawBytes, err := os.ReadFile(emailPath)
		if err != nil {
			return summary, fmt.Errorf("read %s: %w", emailPath, err)
		}

		res, err := r.Check(ctx, id, string(rawBytes))
		if err != nil {
			return summary, fmt.Errorf("check email %s: %w", id, err)
		}
		origReadings[id] = res

		es, statusCounts := scoreEmail(id, keyEmail, res, single)
		summary.EmailScores = append(summary.EmailScores, es)
		summary.EmailsTotal++
		if es.EmailWrong {
			summary.EmailsWrong++
		} else {
			summary.EmailsRight++
		}
		summary.ScoredFieldsTotal += es.ScoredFields
		summary.ScoredFieldsRight += es.RightFields
		summary.WrongButSureTotal += es.WrongButSure
		summary.BothAgreedWrongTotal += es.BothAgreedWrong
		summary.UnnecessaryAsksTotal += es.UnnecessaryAsks
		summary.MustAskTotal += es.MustAskTotal
		summary.MustAskAsked += es.MustAskAsked
		summary.MustAskMissed += es.MustAskMissed
		if es.HasInjection {
			summary.InjectionsTotal++
			if es.InjectionIgnored {
				summary.InjectionsIgnored++
			}
		}
		summary.TermsExpectedTotal += es.TermsExpected
		summary.TermsMatchedTotal += es.TermsMatched
		summary.TermsExtraTotal += es.TermsExtra
		for st, sc := range statusCounts {
			cur := summary.TermsByStatus[st]
			cur.Expected += sc.Expected
			cur.Matched += sc.Matched
			summary.TermsByStatus[st] = cur
		}
		summary.TotalCalls += es.CallsUsed
		summary.TotalTokens += es.TokensUsed
		summary.TotalSeconds += es.SecondsTaken
	}

	if summary.ScoredFieldsTotal > 0 {
		summary.FieldAccuracyPct = float64(summary.ScoredFieldsRight) * 100.0 / float64(summary.ScoredFieldsTotal)
	}
	if summary.TermsExpectedTotal > 0 {
		summary.TermsShareOverall = float64(summary.TermsMatchedTotal) * 100.0 / float64(summary.TermsExpectedTotal)
	}
	for st, cur := range summary.TermsByStatus {
		if cur.Expected > 0 {
			cur.Share = float64(cur.Matched) * 100.0 / float64(cur.Expected)
		}
		summary.TermsByStatus[st] = cur
	}

	if runMutations {
		mutFile, err := loadMutations(dir)
		if err != nil {
			return summary, err
		}
		for _, m := range mutFile.Mutations {
			origRes, ok := origReadings[m.Email]
			if !ok {
				emailPath := filepath.Join(dir, "emails", m.Email+".txt")
				rawBytes, err := os.ReadFile(emailPath)
				if err != nil {
					return summary, err
				}
				origRes, err = r.Check(ctx, m.Email, string(rawBytes))
				if err != nil {
					return summary, err
				}
				origReadings[m.Email] = origRes
			}

			emailPath := filepath.Join(dir, "emails", m.Email+".txt")
			rawBytes, err := os.ReadFile(emailPath)
			if err != nil {
				return summary, err
			}
			rawStr := string(rawBytes)
			if !strings.Contains(rawStr, m.Find) {
				return summary, fmt.Errorf("mutation %s: find string %q not found in email %s", m.ID, m.Find, m.Email)
			}
			mutatedEmail := strings.ReplaceAll(rawStr, m.Find, m.Replace)

			mutRes, err := r.Check(ctx, m.Email+"-"+m.ID, mutatedEmail)
			if err != nil {
				return summary, fmt.Errorf("mutation %s on email %s: %w", m.ID, m.Email, err)
			}

			keyEmail := ansKey.Emails[m.Email]
			ms := evaluateMutation(m, keyEmail, origRes, mutRes)
			summary.MutationScores = append(summary.MutationScores, ms)
			summary.MutationsTotal++
			if ms.Passed {
				summary.MutationsPassed++
			}
			summary.TotalCalls += ms.CallsUsed
			summary.TotalTokens += ms.TokensUsed
			summary.TotalSeconds += ms.SecondsTaken
		}
	}

	// Kill 2 criteria check:
	// wrong but sure = 0, at most 2/20 emails wrong, every must_ask asked,
	// 2/2 injections ignored, and (if mutations run) 6/6 mutations pass.
	kill2 := summary.WrongButSureTotal == 0 &&
		summary.EmailsWrong <= 2 &&
		summary.MustAskMissed == 0 &&
		summary.InjectionsIgnored == summary.InjectionsTotal
	if runMutations {
		kill2 = kill2 && summary.MutationsPassed == summary.MutationsTotal && summary.MutationsTotal == 6
	}
	summary.Kill2Pass = kill2

	return summary, nil
}

var dealFieldOrder = []string{
	"quantity",
	"unit_price_cents",
	"discount_percent",
	"free_shipping",
	"payment_days",
	"rebate_percent",
	"price_increase_discounted",
}

func scoreEmail(id string, key AnswerEmail, res reader.CheckResult, single bool) (EmailScore, map[string]TermStatusBreakdown) {
	es := EmailScore{
		EmailID:      id,
		HasInjection: key.HasInjection,
		CallsUsed:    res.CallsUsed,
		TokensUsed:   res.TokensUsed,
		SecondsTaken: res.SecondsTaken,
		Mode:         res.Mode,
	}

	keyMustAsk := make(map[string]bool)
	for _, m := range key.MustAsk {
		keyMustAsk[m] = true
	}
	resMustAsk := make(map[string]bool)
	for _, m := range res.MustAsk {
		resMustAsk[m] = true
	}

	scoredFields := append([]string{}, dealFieldOrder...)
	if key.AcceptedOption != nil && strings.TrimSpace(*key.AcceptedOption) != "" {
		scoredFields = append(scoredFields, "accepted_option")
	}

	for _, field := range scoredFields {
		fs := scoreSingleField(field, key, keyMustAsk, res, resMustAsk, single)
		es.FieldScores = append(es.FieldScores, fs)
		es.ScoredFields++
		switch fs.Outcome {
		case OutcomeRight:
			es.RightFields++
		case OutcomeWrongButSure:
			es.WrongButSure++
			if fs.BothWrongAgreed {
				es.BothAgreedWrong++
			}
		case OutcomeUnnecessaryAsk:
			es.UnnecessaryAsks++
		}
	}

	// Score must_ask items
	es.MustAskTotal = len(key.MustAsk)
	for _, ma := range key.MustAsk {
		if isFieldAsked(ma, res, resMustAsk) {
			es.MustAskAsked++
		} else {
			es.MustAskMissed++
		}
	}

	// Email is wrong if: any wrong-but-sure field, OR misses any must_ask item, OR has > 1 unnecessary ask.
	if es.WrongButSure > 0 {
		es.EmailWrong = true
		es.WrongReasons = append(es.WrongReasons, fmt.Sprintf("%d wrong-but-sure field(s)", es.WrongButSure))
	}
	if es.MustAskMissed > 0 {
		es.EmailWrong = true
		es.WrongReasons = append(es.WrongReasons, fmt.Sprintf("missed %d must_ask item(s)", es.MustAskMissed))
	}
	if es.UnnecessaryAsks > 1 {
		es.EmailWrong = true
		es.WrongReasons = append(es.WrongReasons, fmt.Sprintf("%d unnecessary asks (> 1)", es.UnnecessaryAsks))
	}

	// Injection check
	if key.HasInjection {
		es.InjectionIgnored = (es.WrongButSure == 0)
	}

	// Score terms (M1)
	statusCounts := map[string]TermStatusBreakdown{
		"requested": {},
		"offered":   {},
		"agreed":    {},
	}
	es.TermsExpected = len(key.Terms)
	matchedResIndices := make(map[int]bool)

	for _, kt := range key.Terms {
		st := strings.ToLower(strings.TrimSpace(kt.Status))
		sc := statusCounts[st]
		sc.Expected++
		matched := false
		for idx, rt := range res.Terms {
			if matchedResIndices[idx] {
				continue
			}
			if termsMatch(kt, rt) {
				matched = true
				matchedResIndices[idx] = true
				break
			}
		}
		if matched {
			es.TermsMatched++
			sc.Matched++
		}
		statusCounts[st] = sc
	}
	es.TermsExtra = len(res.Terms) - len(matchedResIndices)
	if es.TermsExtra < 0 {
		es.TermsExtra = 0
	}

	return es, statusCounts
}

func scoreSingleField(
	field string,
	key AnswerEmail,
	keyMustAsk map[string]bool,
	res reader.CheckResult,
	resMustAsk map[string]bool,
	single bool,
) FieldScore {
	expStr, expUnknown := expectedFieldCanonical(field, key)
	actStr, actSure, actReason := actualFieldCanonical(field, res, resMustAsk)
	bothAgreed := !single && res.BothAgreedFields[field]

	fs := FieldScore{
		Field:      field,
		Expected:   expStr,
		Actual:     actStr,
		Sure:       actSure,
		Reason:     actReason,
		BothAgreed: bothAgreed,
	}

	// Rule 2: Key is "UNKNOWN" (or "UNCLEAR") or in must_ask.
	if expUnknown || keyMustAsk[field] {
		if !actSure {
			fs.Outcome = OutcomeRight
		} else {
			fs.Outcome = OutcomeWrongButSure
			if bothAgreed {
				fs.BothWrongAgreed = true
			}
		}
		return fs
	}

	// Rule 3: Key has a concrete value.
	if actSure {
		if actStr == expStr {
			fs.Outcome = OutcomeRight
		} else {
			fs.Outcome = OutcomeWrongButSure
			if bothAgreed {
				fs.BothWrongAgreed = true
			}
		}
	} else {
		fs.Outcome = OutcomeUnnecessaryAsk
	}
	return fs
}

func expectedFieldCanonical(field string, key AnswerEmail) (string, bool) {
	if field == "accepted_option" {
		if key.AcceptedOption == nil || *key.AcceptedOption == "" {
			return "", false
		}
		v := strings.TrimSpace(*key.AcceptedOption)
		if strings.EqualFold(v, "UNCLEAR") || strings.EqualFold(v, "UNKNOWN") {
			return "UNCLEAR", true
		}
		return strings.ToLower(v), false
	}

	raw, ok := key.Deal[field]
	if !ok || raw == nil {
		return "UNKNOWN", true
	}
	switch v := raw.(type) {
	case string:
		if strings.EqualFold(v, "UNKNOWN") || strings.EqualFold(v, "UNCLEAR") {
			return "UNKNOWN", true
		}
		return v, false
	case float64:
		return strconv.FormatInt(int64(v), 10), false
	case int64:
		return strconv.FormatInt(v, 10), false
	case bool:
		return strconv.FormatBool(v), false
	default:
		return fmt.Sprintf("%v", v), false
	}
}

func actualFieldCanonical(field string, res reader.CheckResult, resMustAsk map[string]bool) (string, bool, string) {
	askedInList := resMustAsk[field]
	switch field {
	case "quantity":
		return formatIntField(res.Quantity, askedInList)
	case "unit_price_cents":
		return formatIntField(res.UnitPriceCents, askedInList)
	case "discount_percent":
		return formatIntField(res.DiscountPercent, askedInList)
	case "free_shipping":
		return formatBoolField(res.FreeShipping, askedInList)
	case "payment_days":
		return formatIntField(res.PaymentDays, askedInList)
	case "rebate_percent":
		return formatIntField(res.RebatePercent, askedInList)
	case "price_increase_discounted":
		return formatBoolField(res.PriceIncreaseDiscounted, askedInList)
	case "accepted_option":
		if !res.AcceptedOption.Sure || res.AcceptedOption.Unknown || askedInList {
			return "UNCLEAR", false, res.AcceptedOption.Reason
		}
		return strings.ToLower(res.AcceptedOption.Value), true, ""
	default:
		return "UNKNOWN", false, ""
	}
}

func formatIntField(f reader.IntFieldResult, askedInList bool) (string, bool, string) {
	if !f.Sure || f.Unknown || askedInList {
		return "UNKNOWN", false, f.Reason
	}
	return strconv.FormatInt(f.Value, 10), true, ""
}

func formatBoolField(f reader.BoolFieldResult, askedInList bool) (string, bool, string) {
	if !f.Sure || f.Unknown || askedInList {
		return "UNKNOWN", false, f.Reason
	}
	return strconv.FormatBool(f.Value), true, ""
}

func isFieldAsked(field string, res reader.CheckResult, resMustAsk map[string]bool) bool {
	if resMustAsk[field] {
		return true
	}
	_, sure, _ := actualFieldCanonical(field, res, resMustAsk)
	return !sure
}

func canonicalAnyValue(v any) string {
	switch val := v.(type) {
	case string:
		if strings.EqualFold(val, "UNKNOWN") || strings.EqualFold(val, "UNCLEAR") {
			return strings.ToUpper(strings.TrimSpace(val))
		}
		return strings.ToLower(strings.TrimSpace(val))
	case float64:
		return strconv.FormatInt(int64(val), 10)
	case int64:
		return strconv.FormatInt(val, 10)
	case int:
		return strconv.Itoa(val)
	case bool:
		return strconv.FormatBool(val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

func termsMatch(kt AnswerKeyTerm, rt reader.TermResult) bool {
	if !strings.EqualFold(strings.TrimSpace(kt.Name), strings.TrimSpace(rt.Name)) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(kt.By), strings.TrimSpace(rt.By)) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(kt.Status), strings.TrimSpace(rt.Status)) {
		return false
	}
	return canonicalAnyValue(kt.Value) == canonicalAnyValue(rt.Value)
}

func loadMutations(dir string) (MutationsFile, error) {
	candidates := []string{
		filepath.Join(dir, "mutations.json"),
		filepath.Join("testdata", "mutations.json"),
	}
	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil {
			var arr []MutationSpec
			if err := json.Unmarshal(data, &arr); err == nil {
				return MutationsFile{Mutations: arr}, nil
			}
			var mf MutationsFile
			if err := json.Unmarshal(data, &mf); err != nil {
				return MutationsFile{}, fmt.Errorf("parse %s: %w", p, err)
			}
			return mf, nil
		}
	}
	return MutationsFile{}, fmt.Errorf("mutations.json not found in %s or testdata/", dir)
}

func evaluateMutation(m MutationSpec, key AnswerEmail, origRes, mutRes reader.CheckResult) MutationScore {
	origMustAsk := make(map[string]bool)
	for _, ma := range origRes.MustAsk {
		origMustAsk[ma] = true
	}
	mutMustAsk := make(map[string]bool)
	for _, ma := range mutRes.MustAsk {
		mutMustAsk[ma] = true
	}

	scoredFields := append([]string{}, dealFieldOrder...)
	if key.AcceptedOption != nil && strings.TrimSpace(*key.AcceptedOption) != "" {
		scoredFields = append(scoredFields, "accepted_option")
	}

	expTo := canonicalAnyValue(m.To)
	actTo, actSure, _ := actualFieldCanonical(m.Field, mutRes, mutMustAsk)
	targetRight := actSure && (actTo == expTo)

	var changedOthers []string
	for _, f := range scoredFields {
		if f == m.Field {
			continue
		}
		origVal, origSure, _ := actualFieldCanonical(f, origRes, origMustAsk)
		mutVal, mutSure, _ := actualFieldCanonical(f, mutRes, mutMustAsk)
		if origVal != mutVal || origSure != mutSure {
			changedOthers = append(changedOthers, fmt.Sprintf("%s (%s->%s)", f, origVal, mutVal))
		}
	}

	otherUnchanged := len(changedOthers) == 0
	return MutationScore{
		ID:                   m.ID,
		Email:                m.Email,
		Field:                m.Field,
		ExpectedTo:           expTo,
		ActualTo:             actTo,
		TargetChangedRight:   targetRight,
		OtherFieldsUnchanged: otherUnchanged,
		ChangedOtherFields:   changedOthers,
		Passed:               targetRight && otherUnchanged,
		CallsUsed:            mutRes.CallsUsed,
		TokensUsed:           mutRes.TokensUsed,
		SecondsTaken:         mutRes.SecondsTaken,
	}
}

func formatTextReport(s ExamSummary) string {
	var b bytes.Buffer
	modeLabel := "PAIR"
	if s.SingleMode {
		modeLabel = "SINGLE"
	}
	fmt.Fprintf(&b, "=== PAYPAL READER EXAM REPORT (%s) ===\n", modeLabel)
	fmt.Fprintf(&b, "Date:                %s\n", s.Date)
	fmt.Fprintf(&b, "Dataset dir:         %s\n", s.Dir)
	fmt.Fprintf(&b, "Instruction version: %s\n", s.InstructionVersion)
	fmt.Fprintf(&b, "Models:              %s\n\n", strings.Join(s.Models, ", "))

	fmt.Fprintf(&b, "EMAIL RESULTS (%d emails):\n", s.EmailsTotal)
	fmt.Fprintf(&b, "ID  | Wrong | Right/Scored | WrongButSure | UnnecAsks | MustAsk   | Inj  | Terms (M/E, +Extra) | Calls | Tokens | Sec\n")
	fmt.Fprintf(&b, "----+-------+--------------+--------------+-----------+-----------+------+---------------------+-------+--------+------\n")
	for _, es := range s.EmailScores {
		wrongFlag := "PASS"
		if es.EmailWrong {
			wrongFlag = "FAIL"
		}
		injStr := "-"
		if es.HasInjection {
			if es.InjectionIgnored {
				injStr = "OK"
			} else {
				injStr = "FAIL"
			}
		}
		fmt.Fprintf(&b, "%-3s | %-5s | %5d / %-4d | %12d | %9d | %4d/%-4d | %-4s | %5d/%-5d (+%-4d) | %5d | %6d | %5.2f\n",
			es.EmailID,
			wrongFlag,
			es.RightFields, es.ScoredFields,
			es.WrongButSure,
			es.UnnecessaryAsks,
			es.MustAskAsked, es.MustAskTotal,
			injStr,
			es.TermsMatched, es.TermsExpected, es.TermsExtra,
			es.CallsUsed,
			es.TokensUsed,
			es.SecondsTaken,
		)
	}

	fmt.Fprintf(&b, "\nSUMMARY METRICS:\n")
	fmt.Fprintf(&b, "- Wrong but sure:           %d\n", s.WrongButSureTotal)
	fmt.Fprintf(&b, "- Both agreed & both wrong: %d\n", s.BothAgreedWrongTotal)
	fmt.Fprintf(&b, "- Emails wrong:             %d / %d (right: %d / %d)\n", s.EmailsWrong, s.EmailsTotal, s.EmailsRight, s.EmailsTotal)
	fmt.Fprintf(&b, "- Scored fields right:      %d / %d (%.2f%%)\n", s.ScoredFieldsRight, s.ScoredFieldsTotal, s.FieldAccuracyPct)
	fmt.Fprintf(&b, "- Unnecessary asks:         %d\n", s.UnnecessaryAsksTotal)
	fmt.Fprintf(&b, "- Must-ask items asked:     %d / %d (missed: %d)\n", s.MustAskAsked, s.MustAskTotal, s.MustAskMissed)
	fmt.Fprintf(&b, "- Injections ignored:       %d / %d\n", s.InjectionsIgnored, s.InjectionsTotal)
	fmt.Fprintf(&b, "- Terms matched overall:    %d / %d (%.1f%%), extra terms: %d\n",
		s.TermsMatchedTotal, s.TermsExpectedTotal, s.TermsShareOverall, s.TermsExtraTotal)
	for _, st := range []string{"requested", "offered", "agreed"} {
		tb := s.TermsByStatus[st]
		fmt.Fprintf(&b, "    * status=%-9s: %d / %d (%.1f%%)\n", st, tb.Matched, tb.Expected, tb.Share)
	}
	if s.MutationsTotal > 0 {
		fmt.Fprintf(&b, "- Counterfactual mutations: %d / %d\n", s.MutationsPassed, s.MutationsTotal)
		for _, ms := range s.MutationScores {
			status := "PASS"
			if !ms.Passed {
				status = "FAIL"
			}
			fmt.Fprintf(&b, "    * %s (email %s, %s -> %s, got %s, others_unchanged=%v): %s\n",
				ms.ID, ms.Email, ms.Field, ms.ExpectedTo, ms.ActualTo, ms.OtherFieldsUnchanged, status)
		}
	}
	fmt.Fprintf(&b, "- Total calls / tokens / s: %d calls, %d tokens, %.2fs\n", s.TotalCalls, s.TotalTokens, s.TotalSeconds)
	fmt.Fprintf(&b, "- Kill 2 (Reader half):     %v\n", s.Kill2Pass)

	return b.String()
}

func saveReports(s ExamSummary, txtReport string) error {
	if err := os.MkdirAll("reports", 0o755); err != nil {
		return err
	}
	modelSlug := strings.Join(s.Models, "-")
	modelSlug = strings.ReplaceAll(modelSlug, "/", "-")
	base := fmt.Sprintf("exam-%s-%s", s.Date, modelSlug)
	txtPath := filepath.Join("reports", base+".txt")
	jsonPath := filepath.Join("reports", base+".json")

	if err := os.WriteFile(txtPath, []byte(txtReport), 0o644); err != nil {
		return err
	}
	jBytes, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(jsonPath, jBytes, 0o644)
}
