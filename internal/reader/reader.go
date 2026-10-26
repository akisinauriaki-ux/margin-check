// Package reader implements the UNDERSTAND stage of the margin-check pipeline.
// It runs two independent model readings (or one in single-model benchmark mode),
// masks PII before sending the email, wraps the email in <untrusted_email> tags,
// verifies every quote and normalises every value in code, retries at most once
// per model on verifier failure, computes "sure" strictly in code, enforces
// per-check call/time/token budgets, and writes a JSON trace per check.
package reader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"margincheck/internal/gemini"
	"margincheck/internal/normalize"
	"margincheck/internal/quote"
)

const (
	// DefaultMaxCallsPerCheck is the hard cap on model calls per email check (OWASP LLM10).
	DefaultMaxCallsPerCheck = 4
	// DefaultMaxSecondsPerCheck is the hard cap on wall-clock seconds per email check.
	DefaultMaxSecondsPerCheck = 120
	// DefaultMaxTokensPerCheck is the hard cap on total tokens per email check.
	DefaultMaxTokensPerCheck = 60000
)

// Reason codes for why a field is marked as not sure (ask the seller).
const (
	ReasonReadingsDisagree = "readings_disagree"
	ReasonQuoteNotInEmail  = "quote_not_in_email"
	ReasonValueCantParse   = "value_cant_parse"
	ReasonModelUnknown     = "model_unknown"
	ReasonBudget           = "budget"
)

// Generator is the interface used to call Gemini (implemented by *gemini.Client and test doubles).
type Generator interface {
	Generate(ctx context.Context, req gemini.Request) (gemini.Response, error)
}

// Config configures the Reader.
type Config struct {
	Client            Generator
	PromptPath        string
	PromptVersion     string
	ModelA            string
	ModelB            string
	Single            bool
	TraceDir          string
	MaxCallsPerCheck  int
	MaxSecondsPerCheck int
	MaxTokensPerCheck int
}

// RawField is the JSON shape returned by the model for a single deal field.
type RawField struct {
	Value          string `json:"value"`
	SourceSentence string `json:"source_sentence"`
}

// RawTerm is the JSON shape returned by the model for a single term in the terms list.
type RawTerm struct {
	Name           string `json:"name"`
	Value          string `json:"value"`
	Currency       string `json:"currency"`
	By             string `json:"by"`
	Status         string `json:"status"`
	SourceSentence string `json:"source_sentence"`
}

// RawBuyerInterest is the JSON shape returned by the model for buyer_interest.
type RawBuyerInterest struct {
	Summary         string   `json:"summary"`
	SourceSentences []string `json:"source_sentences"`
}

// RawReading is the exact JSON schema produced by a single model reading.
type RawReading struct {
	Quantity                RawField         `json:"quantity"`
	UnitPriceCents          RawField         `json:"unit_price_cents"`
	DiscountPercent         RawField         `json:"discount_percent"`
	FreeShipping            RawField         `json:"free_shipping"`
	PaymentDays             RawField         `json:"payment_days"`
	RebatePercent           RawField         `json:"rebate_percent"`
	PriceIncreaseDiscounted RawField         `json:"price_increase_discounted"`
	AcceptedOption          RawField         `json:"accepted_option"`
	MustAsk                 []string         `json:"must_ask"`
	Terms                   []RawTerm        `json:"terms"`
	BuyerInterest           RawBuyerInterest `json:"buyer_interest"`
}

// IntFieldResult holds the resolved result for an integer deal field.
type IntFieldResult struct {
	Value   int64  `json:"value"`
	Unknown bool   `json:"unknown"`
	Sure    bool   `json:"sure"`
	Reason  string `json:"reason,omitempty"`
	Quote   string `json:"quote"`
}

// BoolFieldResult holds the resolved result for a boolean deal field.
type BoolFieldResult struct {
	Value   bool   `json:"value"`
	Unknown bool   `json:"unknown"`
	Sure    bool   `json:"sure"`
	Reason  string `json:"reason,omitempty"`
	Quote   string `json:"quote"`
}

// StringFieldResult holds the resolved result for a string deal field (accepted_option).
type StringFieldResult struct {
	Value   string `json:"value"`
	Unknown bool   `json:"unknown"`
	Sure    bool   `json:"sure"`
	Reason  string `json:"reason,omitempty"`
	Quote   string `json:"quote"`
}

// TermResult holds a verified, normalised term from the email thread.
type TermResult struct {
	Name           string `json:"name"`
	Value          any    `json:"value"`
	Currency       string `json:"currency,omitempty"`
	By             string `json:"by"`
	Status         string `json:"status"`
	SourceSentence string `json:"source_sentence"`
}

// CheckResult is the final output of reading an email thread.
type CheckResult struct {
	EmailID                 string            `json:"email_id,omitempty"`
	EmailSHA256             string            `json:"email_sha256"`
	Quantity                IntFieldResult    `json:"quantity"`
	UnitPriceCents          IntFieldResult    `json:"unit_price_cents"`
	DiscountPercent         IntFieldResult    `json:"discount_percent"`
	FreeShipping            BoolFieldResult   `json:"free_shipping"`
	PaymentDays             IntFieldResult    `json:"payment_days"`
	RebatePercent           IntFieldResult    `json:"rebate_percent"`
	PriceIncreaseDiscounted BoolFieldResult   `json:"price_increase_discounted"`
	AcceptedOption          StringFieldResult `json:"accepted_option"`
	MustAsk                 []string          `json:"must_ask"`
	Terms                   []TermResult      `json:"terms"`
	BuyerInterest           RawBuyerInterest  `json:"buyer_interest"`
	BothAgreedFields        map[string]bool   `json:"both_agreed_fields"`
	BudgetExceeded          bool              `json:"budget_exceeded"`
	CallsUsed               int               `json:"calls_used"`
	TokensUsed              int               `json:"tokens_used"`
	SecondsTaken            float64           `json:"seconds_taken"`
	Mode                    string            `json:"mode"` // "live" or "replayed"
	TracePath               string            `json:"trace_path,omitempty"`
}

// TraceCall records a single model call within a check trace.
type TraceCall struct {
	Model          string      `json:"model"`
	ModelVersion   string      `json:"model_version"`
	Attempt        int         `json:"attempt"`
	Cached         bool        `json:"cached"`
	LatencyMs      int64       `json:"latency_ms"`
	PromptTokens   int         `json:"prompt_tokens"`
	OutputTokens   int         `json:"output_tokens"`
	TotalTokens    int         `json:"total_tokens"`
	RawReply       string      `json:"raw_reply"`
	ParsedReading  *RawReading `json:"parsed_reading,omitempty"`
	VerifierErrors []string    `json:"verifier_errors,omitempty"`
	Error          string      `json:"error,omitempty"`
}

// TraceFile is the full per-check audit record written to margin-check/reports/traces/.
type TraceFile struct {
	EmailID            string       `json:"email_id,omitempty"`
	EmailSHA256        string       `json:"email_sha256"`
	InstructionVersion string       `json:"instruction_version"`
	Models             []string     `json:"models"`
	ModelVersions      []string     `json:"model_versions"`
	Temperature        int          `json:"temperature"`
	Calls              []TraceCall  `json:"calls"`
	FinalResult        CheckResult  `json:"final_result"`
	SecondsTaken       float64      `json:"seconds_taken"`
	TokensUsed         int          `json:"tokens_used"`
	Mode               string       `json:"mode"` // "live" or "replayed"
	TimestampUTC       string       `json:"timestamp_utc"`
}

// Reader orchestrates prompt loading, PII masking, two-model execution, verification, and trace writing.
type Reader struct {
	cfg               Config
	systemInstruction string
	responseSchema    map[string]any
}

// New creates a Reader and loads the prompt file and JSON schema.
func New(cfg Config) (*Reader, error) {
	if cfg.Client == nil {
		return nil, errors.New("reader: Client is required")
	}
	if cfg.PromptPath == "" {
		return nil, errors.New("reader: PromptPath is required")
	}
	if cfg.ModelA == "" {
		return nil, errors.New("reader: ModelA is required")
	}
	if !cfg.Single && cfg.ModelB == "" {
		return nil, errors.New("reader: ModelB is required when Single is false")
	}
	if cfg.MaxCallsPerCheck <= 0 {
		cfg.MaxCallsPerCheck = DefaultMaxCallsPerCheck
	}
	if cfg.MaxSecondsPerCheck <= 0 {
		cfg.MaxSecondsPerCheck = DefaultMaxSecondsPerCheck
	}
	if cfg.MaxTokensPerCheck <= 0 {
		cfg.MaxTokensPerCheck = DefaultMaxTokensPerCheck
	}
	if cfg.PromptVersion == "" {
		base := filepath.Base(cfg.PromptPath)
		cfg.PromptVersion = strings.TrimSuffix(base, filepath.Ext(base))
	}

	rawPrompt, err := os.ReadFile(cfg.PromptPath)
	if err != nil {
		return nil, fmt.Errorf("reader: read prompt %s: %w", cfg.PromptPath, err)
	}
	promptStr := string(rawPrompt)
	schema, schemaRaw, err := extractSchemaFromPrompt(promptStr)
	if err != nil {
		return nil, fmt.Errorf("reader: %w", err)
	}

	// Ensure schema contains no confidence field of any kind (M4).
	if strings.Contains(strings.ToLower(schemaRaw), `"confidence"`) {
		return nil, errors.New("reader: response schema must not contain a confidence field")
	}

	return &Reader{
		cfg:               cfg,
		systemInstruction: promptStr,
		responseSchema:    schema,
	}, nil
}

var reSchemaBlock = regexp.MustCompile("(?s)## Output Schema\\s*```json\\s*(\\{.*?\\})\\s*```")

func extractSchemaFromPrompt(promptText string) (map[string]any, string, error) {
	m := reSchemaBlock.FindStringSubmatch(promptText)
	if m == nil {
		return nil, "", errors.New("could not find ## Output Schema ```json block in prompt file")
	}
	var js map[string]any
	if err := json.Unmarshal([]byte(m[1]), &js); err != nil {
		return nil, "", fmt.Errorf("invalid JSON in ## Output Schema block: %w", err)
	}
	return js, m[1], nil
}

// StripMetadataLine removes a leading "Based on: ..." line (and following blank lines) before reading.
func StripMetadataLine(rawEmail string) string {
	norm := strings.ReplaceAll(rawEmail, "\r\n", "\n")
	lines := strings.Split(norm, "\n")
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "Based on:") {
		lines = lines[1:]
		for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// PII masking patterns (S2: email addresses, phone numbers, and web links).
var (
	reEmailPII = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	reURLPII   = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"]*[^\s<>".?!,;:\)\]]`)
	rePhonePII = regexp.MustCompile(`(?:\+\d{1,3}[\s.\-]?)?\(\d{3}\)[\s.\-]?\d{3}[\s.\-]\d{4}\b|(?:\+\d{1,3}[\s.\-]?)?\b\d{3}[\s.\-]\d{3}[\s.\-]\d{4}\b`)
)

// PIIMap stores placeholders and their original values so quotes can be restored after reading.
type PIIMap struct {
	tokenToOrig map[string]string
}

// MaskPII replaces email addresses, web URLs, and phone numbers with deterministic tokens.
func MaskPII(text string) (string, *PIIMap) {
	pm := &PIIMap{tokenToOrig: make(map[string]string)}
	origToToken := make(map[string]string)

	replaceWithPrefix := func(re *regexp.Regexp, prefix string, src string) string {
		count := 0
		return re.ReplaceAllStringFunc(src, func(match string) string {
			if tok, ok := origToToken[match]; ok {
				return tok
			}
			count++
			tok := fmt.Sprintf("[%s_%d]", prefix, count)
			origToToken[match] = tok
			pm.tokenToOrig[tok] = match
			return tok
		})
	}

	masked := replaceWithPrefix(reURLPII, "URL", text)
	masked = replaceWithPrefix(reEmailPII, "EMAIL", masked)
	masked = replaceWithPrefix(rePhonePII, "PHONE", masked)
	return masked, pm
}

// Unmask restores any PII tokens in s back to their original values.
func (pm *PIIMap) Unmask(s string) string {
	if pm == nil || len(pm.tokenToOrig) == 0 || s == "" {
		return s
	}
	out := s
	for tok, orig := range pm.tokenToOrig {
		out = strings.ReplaceAll(out, tok, orig)
	}
	return out
}

func (pm *PIIMap) unmaskReading(r *RawReading) {
	if pm == nil || r == nil {
		return
	}
	unmaskField := func(f *RawField) {
		f.Value = pm.Unmask(f.Value)
		f.SourceSentence = pm.Unmask(f.SourceSentence)
	}
	unmaskField(&r.Quantity)
	unmaskField(&r.UnitPriceCents)
	unmaskField(&r.DiscountPercent)
	unmaskField(&r.FreeShipping)
	unmaskField(&r.PaymentDays)
	unmaskField(&r.RebatePercent)
	unmaskField(&r.PriceIncreaseDiscounted)
	unmaskField(&r.AcceptedOption)
	for i := range r.Terms {
		r.Terms[i].Value = pm.Unmask(r.Terms[i].Value)
		r.Terms[i].SourceSentence = pm.Unmask(r.Terms[i].SourceSentence)
	}
	r.BuyerInterest.Summary = pm.Unmask(r.BuyerInterest.Summary)
	for i := range r.BuyerInterest.SourceSentences {
		r.BuyerInterest.SourceSentences[i] = pm.Unmask(r.BuyerInterest.SourceSentences[i])
	}
}

// parsedIntField holds the verified state of an integer field from one reading.
type parsedIntField struct {
	val     int64
	unknown bool
	valid   bool
	reason  string
	quote   string
}

// parsedBoolField holds the verified state of a boolean field from one reading.
type parsedBoolField struct {
	val     bool
	unknown bool
	valid   bool
	reason  string
	quote   string
}

// parsedStrField holds the verified state of a string field from one reading.
type parsedStrField struct {
	val     string
	unknown bool
	valid   bool
	reason  string
	quote   string
}

type verifiedReading struct {
	raw                     RawReading
	quantity                parsedIntField
	unitPriceCents          parsedIntField
	discountPercent         parsedIntField
	freeShipping            parsedBoolField
	paymentDays             parsedIntField
	rebatePercent           parsedIntField
	priceIncreaseDiscounted parsedBoolField
	acceptedOption          parsedStrField
	mustAsk                 map[string]bool
	terms                   []TermResult
	buyerInterest           RawBuyerInterest
	verifierErrors          []string
}

// Check runs the reader pipeline on rawEmail and writes a trace file if TraceDir is configured.
func (r *Reader) Check(ctx context.Context, emailID, rawEmail string) (CheckResult, error) {
	start := time.Now()
	sum := sha256.Sum256([]byte(rawEmail))
	emailHash := hex.EncodeToString(sum[:])

	cleanEmail := StripMetadataLine(rawEmail)
	maskedEmail, piiMap := MaskPII(cleanEmail)
	wrappedEmail := "<untrusted_email>\n" + maskedEmail + "\n</untrusted_email>"

	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.MaxSecondsPerCheck)*time.Second)
	defer cancel()

	var (
		callsUsed     int
		tokensUsed    int
		anyLiveCall   bool
		budgetHit     bool
		traceCalls    []TraceCall
		modelVersions []string
	)

	checkBudget := func() bool {
		if time.Since(start) > time.Duration(r.cfg.MaxSecondsPerCheck)*time.Second {
			budgetHit = true
			return false
		}
		if checkCtx.Err() != nil {
			budgetHit = true
			return false
		}
		if tokensUsed > r.cfg.MaxTokensPerCheck {
			budgetHit = true
			return false
		}
		return true
	}

	runModelWithRetry := func(modelName string) (verifiedReading, error) {
		var lastVerified verifiedReading
		for attempt := 1; attempt <= 2; attempt++ {
			if !checkBudget() {
				return lastVerified, nil
			}
			if callsUsed >= r.cfg.MaxCallsPerCheck {
				budgetHit = true
				return lastVerified, nil
			}

			userText := "Read the following email thread and extract the deal fields, terms, must_ask, and buyer_interest according to the schema.\n\n" + wrappedEmail
			if attempt == 2 && len(lastVerified.verifierErrors) > 0 {
				userText += "\n\nYour previous response had verification errors:\n- " +
					strings.Join(lastVerified.verifierErrors, "\n- ") +
					"\nFix these exact fields so every source_sentence is copied verbatim from the email and contains the claimed value."
			}

			callsUsed++
			resp, err := r.cfg.Client.Generate(checkCtx, gemini.Request{
				Model:              modelName,
				InstructionVersion: r.cfg.PromptVersion,
				SystemInstruction:  r.systemInstruction,
				UserText:           userText,
				ResponseSchema:     r.responseSchema,
			})
			if err != nil {
				traceCalls = append(traceCalls, TraceCall{
					Model:   modelName,
					Attempt: attempt,
					Error:   err.Error(),
				})
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					budgetHit = true
					return lastVerified, nil
				}
				return lastVerified, err
			}

			if !resp.Replayed {
				anyLiveCall = true
			}
			tokensUsed += resp.TotalTokens
			if resp.ModelVersion != "" {
				modelVersions = appendUnique(modelVersions, resp.ModelVersion)
			}

			var raw RawReading
			parseErr := strictUnmarshalReading(resp.Text, &raw)
			if parseErr != nil {
				vErr := fmt.Sprintf("JSON schema parse error: %v", parseErr)
				lastVerified = verifiedReading{
					verifierErrors:          []string{vErr},
					quantity:                parsedIntField{reason: ReasonValueCantParse},
					unitPriceCents:          parsedIntField{reason: ReasonValueCantParse},
					discountPercent:         parsedIntField{reason: ReasonValueCantParse},
					freeShipping:            parsedBoolField{reason: ReasonValueCantParse},
					paymentDays:             parsedIntField{reason: ReasonValueCantParse},
					rebatePercent:           parsedIntField{reason: ReasonValueCantParse},
					priceIncreaseDiscounted: parsedBoolField{reason: ReasonValueCantParse},
					acceptedOption:          parsedStrField{reason: ReasonValueCantParse},
				}
				traceCalls = append(traceCalls, TraceCall{
					Model:          modelName,
					ModelVersion:   resp.ModelVersion,
					Attempt:        attempt,
					Cached:         resp.Replayed,
					LatencyMs:      resp.Duration.Milliseconds(),
					PromptTokens:   resp.PromptTokens,
					OutputTokens:   resp.CandidateTokens,
					TotalTokens:    resp.TotalTokens,
					RawReply:       resp.Text,
					VerifierErrors: lastVerified.verifierErrors,
				})
				if tokensUsed > r.cfg.MaxTokensPerCheck {
					budgetHit = true
					return lastVerified, nil
				}
				continue
			}

			piiMap.unmaskReading(&raw)
			lastVerified = verifySingleReading(cleanEmail, raw)
			rawCopy := raw
			traceCalls = append(traceCalls, TraceCall{
				Model:          modelName,
				ModelVersion:   resp.ModelVersion,
				Attempt:        attempt,
				Cached:         resp.Replayed,
				LatencyMs:      resp.Duration.Milliseconds(),
				PromptTokens:   resp.PromptTokens,
				OutputTokens:   resp.CandidateTokens,
				TotalTokens:    resp.TotalTokens,
				RawReply:       resp.Text,
				ParsedReading:  &rawCopy,
				VerifierErrors: lastVerified.verifierErrors,
			})

			if tokensUsed > r.cfg.MaxTokensPerCheck {
				budgetHit = true
				return lastVerified, nil
			}
			if len(lastVerified.verifierErrors) == 0 {
				break
			}
		}
		return lastVerified, nil
	}

	readingA, err := runModelWithRetry(r.cfg.ModelA)
	if err != nil {
		return CheckResult{}, err
	}

	var readingB verifiedReading
	if !r.cfg.Single && !budgetHit {
		readingB, err = runModelWithRetry(r.cfg.ModelB)
		if err != nil {
			return CheckResult{}, err
		}
	}

	elapsed := time.Since(start).Seconds()
	if elapsed > float64(r.cfg.MaxSecondsPerCheck) {
		budgetHit = true
	}

	mode := "replayed"
	if anyLiveCall {
		mode = "live"
	}

	var res CheckResult
	if budgetHit {
		res = buildBudgetExceededResult(emailID, emailHash, callsUsed, tokensUsed, elapsed, mode)
	} else if r.cfg.Single {
		res = combineSingleReading(emailID, emailHash, readingA, callsUsed, tokensUsed, elapsed, mode)
	} else {
		res = combineTwoReadings(emailID, emailHash, readingA, readingB, callsUsed, tokensUsed, elapsed, mode)
	}

	if r.cfg.TraceDir != "" {
		models := []string{r.cfg.ModelA}
		if !r.cfg.Single {
			models = append(models, r.cfg.ModelB)
		}
		trace := TraceFile{
			EmailID:            emailID,
			EmailSHA256:        emailHash,
			InstructionVersion: r.cfg.PromptVersion,
			Models:             models,
			ModelVersions:      modelVersions,
			Temperature:        0,
			Calls:              traceCalls,
			FinalResult:        res,
			SecondsTaken:       elapsed,
			TokensUsed:         tokensUsed,
			Mode:               mode,
			TimestampUTC:       time.Now().UTC().Format(time.RFC3339),
		}
		if tracePath, wErr := writeTraceFile(r.cfg.TraceDir, emailID, emailHash, r.cfg.PromptVersion, models, trace); wErr == nil {
			res.TracePath = tracePath
		}
	}

	return res, nil
}

func strictUnmarshalReading(text string, out *RawReading) error {
	trimmed := strings.TrimSpace(text)
	// Reject any output that smuggles a confidence key.
	var rawMap map[string]any
	if err := json.Unmarshal([]byte(trimmed), &rawMap); err != nil {
		return err
	}
	if _, hasConf := rawMap["confidence"]; hasConf {
		return errors.New("output contains forbidden confidence field")
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

func verifySingleReading(cleanEmail string, raw RawReading) verifiedReading {
	vr := verifiedReading{
		raw:      raw,
		mustAsk:  make(map[string]bool),
	}
	for _, m := range raw.MustAsk {
		vr.mustAsk[strings.TrimSpace(m)] = true
	}

	// Verify accepted_option first so other fields know if accepted_option is UNCLEAR.
	vr.acceptedOption = verifyAcceptedOption(cleanEmail, raw.AcceptedOption, vr.mustAsk, &vr.verifierErrors)
	if vr.acceptedOption.unknown {
		vr.mustAsk["accepted_option"] = true
	}

	vr.quantity = verifyIntDealField(cleanEmail, "quantity", raw.Quantity, false, vr.mustAsk["quantity"], &vr.verifierErrors)
	if vr.quantity.unknown {
		vr.mustAsk["quantity"] = true
	}

	vr.unitPriceCents = verifyIntDealField(cleanEmail, "unit_price_cents", raw.UnitPriceCents, false, vr.mustAsk["unit_price_cents"], &vr.verifierErrors)
	if vr.unitPriceCents.unknown {
		vr.mustAsk["unit_price_cents"] = true
	}

	vr.discountPercent = verifyIntDealField(cleanEmail, "discount_percent", raw.DiscountPercent, true, vr.mustAsk["discount_percent"], &vr.verifierErrors)
	if vr.discountPercent.unknown {
		vr.mustAsk["discount_percent"] = true
	}

	vr.freeShipping = verifyBoolDealField(cleanEmail, "free_shipping", raw.FreeShipping, true, vr.mustAsk["free_shipping"], &vr.verifierErrors)
	if vr.freeShipping.unknown {
		vr.mustAsk["free_shipping"] = true
	}

	allowZeroNoQuotePayment := vr.acceptedOption.unknown
	vr.paymentDays = verifyIntDealField(cleanEmail, "payment_days", raw.PaymentDays, allowZeroNoQuotePayment, vr.mustAsk["payment_days"], &vr.verifierErrors)
	if vr.paymentDays.unknown {
		vr.mustAsk["payment_days"] = true
	}

	vr.rebatePercent = verifyIntDealField(cleanEmail, "rebate_percent", raw.RebatePercent, true, vr.mustAsk["rebate_percent"], &vr.verifierErrors)
	if vr.rebatePercent.unknown {
		vr.mustAsk["rebate_percent"] = true
	}

	vr.priceIncreaseDiscounted = verifyBoolDealField(cleanEmail, "price_increase_discounted", raw.PriceIncreaseDiscounted, true, vr.mustAsk["price_increase_discounted"], &vr.verifierErrors)
	if vr.priceIncreaseDiscounted.unknown {
		vr.mustAsk["price_increase_discounted"] = true
	}

	// Verify terms
	for _, rt := range raw.Terms {
		tr, ok := verifyTerm(cleanEmail, rt)
		if ok {
			vr.terms = append(vr.terms, tr)
		}
	}

	// Verify buyer_interest sentences
	var validInterestSentences []string
	for _, s := range raw.BuyerInterest.SourceSentences {
		if res, err := quote.ContainsQuote(cleanEmail, s); err == nil {
			validInterestSentences = append(validInterestSentences, res.Original)
		}
	}
	vr.buyerInterest = RawBuyerInterest{
		Summary:         strings.TrimSpace(raw.BuyerInterest.Summary),
		SourceSentences: validInterestSentences,
	}

	return vr
}

func verifyIntDealField(cleanEmail, field string, rf RawField, allowZeroEmptyQuote bool, inMustAsk bool, errs *[]string) parsedIntField {
	valStr := strings.TrimSpace(rf.Value)
	sent := strings.TrimSpace(rf.SourceSentence)

	if strings.EqualFold(valStr, "UNKNOWN") || strings.EqualFold(valStr, "UNCLEAR") || inMustAsk && valStr == "" {
		if sent != "" {
			if _, err := quote.ContainsQuote(cleanEmail, sent); err != nil {
				*errs = append(*errs, fmt.Sprintf("%s: quote not in email: %q", field, sent))
			}
		}
		return parsedIntField{
			unknown: true,
			valid:   true,
			reason:  ReasonModelUnknown,
			quote:   sent,
		}
	}

	intVal, err := parseNormalizedInt(field, valStr, sent)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: cannot parse value %q: %v", field, valStr, err))
		return parsedIntField{
			valid:  false,
			reason: ReasonValueCantParse,
			quote:  sent,
		}
	}

	if intVal == 0 && allowZeroEmptyQuote && sent == "" {
		return parsedIntField{
			val:    0,
			valid:  true,
			quote:  "",
		}
	}

	qRes, qErr := quote.Verify(cleanEmail, sent, field, intVal, false, "")
	if qErr != nil {
		*errs = append(*errs, fmt.Sprintf("%s: quote verification failed for value %d with quote %q: %v", field, intVal, sent, qErr))
		return parsedIntField{
			val:    intVal,
			valid:  false,
			reason: ReasonQuoteNotInEmail,
			quote:  sent,
		}
	}

	return parsedIntField{
		val:   intVal,
		valid: true,
		quote: qRes.Original,
	}
}

func verifyBoolDealField(cleanEmail, field string, rf RawField, allowFalseEmptyQuote bool, inMustAsk bool, errs *[]string) parsedBoolField {
	valStr := strings.TrimSpace(rf.Value)
	sent := strings.TrimSpace(rf.SourceSentence)

	if strings.EqualFold(valStr, "UNKNOWN") || strings.EqualFold(valStr, "UNCLEAR") || (inMustAsk && valStr == "") {
		if sent != "" {
			if _, err := quote.ContainsQuote(cleanEmail, sent); err != nil {
				*errs = append(*errs, fmt.Sprintf("%s: quote not in email: %q", field, sent))
			}
		}
		return parsedBoolField{
			unknown: true,
			valid:   true,
			reason:  ReasonModelUnknown,
			quote:   sent,
		}
	}

	var boolVal bool
	switch strings.ToLower(valStr) {
	case "true", "yes":
		boolVal = true
	case "false", "no", "":
		boolVal = false
	default:
		*errs = append(*errs, fmt.Sprintf("%s: invalid boolean value %q", field, valStr))
		return parsedBoolField{
			valid:  false,
			reason: ReasonValueCantParse,
			quote:  sent,
		}
	}

	if !boolVal && allowFalseEmptyQuote && sent == "" {
		return parsedBoolField{
			val:   false,
			valid: true,
			quote: "",
		}
	}

	qRes, qErr := quote.Verify(cleanEmail, sent, field, 0, boolVal, "")
	if qErr != nil {
		*errs = append(*errs, fmt.Sprintf("%s: quote verification failed for value %v with quote %q: %v", field, boolVal, sent, qErr))
		return parsedBoolField{
			val:    boolVal,
			valid:  false,
			reason: ReasonQuoteNotInEmail,
			quote:  sent,
		}
	}

	return parsedBoolField{
		val:   boolVal,
		valid: true,
		quote: qRes.Original,
	}
}

func verifyAcceptedOption(cleanEmail string, rf RawField, mustAsk map[string]bool, errs *[]string) parsedStrField {
	valStr := strings.TrimSpace(rf.Value)
	sent := strings.TrimSpace(rf.SourceSentence)

	if valStr == "" || strings.EqualFold(valStr, "null") || strings.EqualFold(valStr, "none") {
		if mustAsk["accepted_option"] {
			return parsedStrField{
				val:     "UNCLEAR",
				unknown: true,
				valid:   true,
				reason:  ReasonModelUnknown,
				quote:   sent,
			}
		}
		return parsedStrField{
			val:   "",
			valid: true,
			quote: "",
		}
	}

	if strings.EqualFold(valStr, "UNCLEAR") || strings.EqualFold(valStr, "UNKNOWN") {
		if sent != "" {
			if _, err := quote.ContainsQuote(cleanEmail, sent); err != nil {
				*errs = append(*errs, fmt.Sprintf("accepted_option: quote not in email: %q", sent))
			}
		}
		return parsedStrField{
			val:     "UNCLEAR",
			unknown: true,
			valid:   true,
			reason:  ReasonModelUnknown,
			quote:   sent,
		}
	}

	normVal := strings.ToLower(valStr)
	switch normVal {
	case "standard", "quantity", "early_payment", "changed":
		qRes, qErr := quote.Verify(cleanEmail, sent, "accepted_option", 0, false, normVal)
		if qErr != nil {
			*errs = append(*errs, fmt.Sprintf("accepted_option: quote verification failed for %q with quote %q: %v", normVal, sent, qErr))
			return parsedStrField{
				val:    normVal,
				valid:  false,
				reason: ReasonQuoteNotInEmail,
				quote:  sent,
			}
		}
		return parsedStrField{
			val:   normVal,
			valid: true,
			quote: qRes.Original,
		}
	default:
		*errs = append(*errs, fmt.Sprintf("accepted_option: invalid option value %q", valStr))
		return parsedStrField{
			valid:  false,
			reason: ReasonValueCantParse,
			quote:  sent,
		}
	}
}

func parseNormalizedInt(field, valStr, quoteSent string) (int64, error) {
	switch field {
	case "unit_price_cents", "shipping_cents":
		// If the model returned an explicit currency string like "$40" or "$40.00", use normalize.ParseMoney.
		if strings.ContainsAny(valStr, "$€£.") {
			cents, _, err := normalize.ParseMoney(valStr)
			if err == nil {
				return cents, nil
			}
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(valStr, ",", ""), 10, 64)
		if err != nil {
			cents, _, mErr := normalize.ParseMoney(valStr)
			if mErr == nil {
				return cents, nil
			}
			return 0, err
		}
		// If n is already in cents (e.g. 4000 for $40) vs dollars (e.g. 40 for $40),
		// check what the quote backs.
		ms := normalize.ExtractMoney(quoteSent)
		for _, m := range ms {
			if m.Int == n {
				return n, nil
			}
		}
		for _, m := range ms {
			if m.Int == n*100 {
				return n * 100, nil
			}
		}
		return n, nil

	case "quantity":
		if n, err := strconv.ParseInt(strings.ReplaceAll(valStr, ",", ""), 10, 64); err == nil {
			return n, nil
		}
		return normalize.ParseQuantity(valStr)

	case "discount_percent", "rebate_percent":
		cleaned := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(valStr), "%"))
		if n, err := strconv.ParseInt(cleaned, 10, 64); err == nil {
			return n, nil
		}
		return normalize.ParsePercent(valStr)

	case "payment_days":
		if n, err := strconv.ParseInt(strings.TrimSpace(valStr), 10, 64); err == nil {
			return n, nil
		}
		return normalize.ParsePaymentDays(valStr)

	default:
		return strconv.ParseInt(strings.TrimSpace(valStr), 10, 64)
	}
}

func verifyTerm(cleanEmail string, rt RawTerm) (TermResult, bool) {
	qRes, err := quote.ContainsQuote(cleanEmail, rt.SourceSentence)
	if err != nil {
		return TermResult{}, false
	}
	name := strings.TrimSpace(rt.Name)
	valStr := strings.TrimSpace(rt.Value)
	by := strings.ToLower(strings.TrimSpace(rt.By))
	status := strings.ToLower(strings.TrimSpace(rt.Status))
	currency := strings.ToUpper(strings.TrimSpace(rt.Currency))

	var parsedVal any
	if strings.EqualFold(valStr, "UNKNOWN") || strings.EqualFold(valStr, "UNCLEAR") {
		parsedVal = strings.ToUpper(valStr)
	} else {
		switch name {
		case "quantity", "discount_percent", "payment_days", "rebate_percent":
			n, err := parseNormalizedInt(name, valStr, qRes.Normalized)
			if err != nil {
				return TermResult{}, false
			}
			parsedVal = n
		case "unit_price_cents", "shipping_cents":
			n, err := parseNormalizedInt(name, valStr, qRes.Normalized)
			if err != nil {
				return TermResult{}, false
			}
			parsedVal = n
			if currency == "" {
				currency = "USD"
			}
		case "free_shipping":
			switch strings.ToLower(valStr) {
			case "true", "yes":
				parsedVal = true
			case "false", "no":
				parsedVal = false
			default:
				return TermResult{}, false
			}
		case "accepted_option":
			parsedVal = strings.ToLower(valStr)
		default:
			parsedVal = valStr
		}
	}

	return TermResult{
		Name:           name,
		Value:          parsedVal,
		Currency:       currency,
		By:             by,
		Status:         status,
		SourceSentence: qRes.Original,
	}, true
}

func buildBudgetExceededResult(emailID, emailHash string, callsUsed, tokensUsed int, elapsed float64, mode string) CheckResult {
	return CheckResult{
		EmailID:                 emailID,
		EmailSHA256:             emailHash,
		Quantity:                IntFieldResult{Unknown: true, Sure: false, Reason: ReasonBudget},
		UnitPriceCents:          IntFieldResult{Unknown: true, Sure: false, Reason: ReasonBudget},
		DiscountPercent:         IntFieldResult{Unknown: true, Sure: false, Reason: ReasonBudget},
		FreeShipping:            BoolFieldResult{Unknown: true, Sure: false, Reason: ReasonBudget},
		PaymentDays:             IntFieldResult{Unknown: true, Sure: false, Reason: ReasonBudget},
		RebatePercent:           IntFieldResult{Unknown: true, Sure: false, Reason: ReasonBudget},
		PriceIncreaseDiscounted: BoolFieldResult{Unknown: true, Sure: false, Reason: ReasonBudget},
		AcceptedOption:          StringFieldResult{Unknown: true, Sure: false, Reason: ReasonBudget},
		MustAsk: []string{
			"quantity",
			"unit_price_cents",
			"discount_percent",
			"free_shipping",
			"payment_days",
			"rebate_percent",
			"price_increase_discounted",
			"accepted_option",
		},
		BothAgreedFields: map[string]bool{},
		BudgetExceeded:   true,
		CallsUsed:        callsUsed,
		TokensUsed:       tokensUsed,
		SecondsTaken:     elapsed,
		Mode:             mode,
	}
}

func combineSingleReading(emailID, emailHash string, a verifiedReading, callsUsed, tokensUsed int, elapsed float64, mode string) CheckResult {
	mustAskSet := make(map[string]bool)
	for k, v := range a.mustAsk {
		if v {
			mustAskSet[k] = true
		}
	}

	resolveInt := func(name string, f parsedIntField) IntFieldResult {
		if f.unknown || mustAskSet[name] {
			mustAskSet[name] = true
			return IntFieldResult{Unknown: true, Sure: false, Reason: ReasonModelUnknown, Quote: f.quote}
		}
		if !f.valid {
			mustAskSet[name] = true
			return IntFieldResult{Unknown: true, Sure: false, Reason: f.reason, Quote: f.quote}
		}
		return IntFieldResult{Value: f.val, Sure: true, Quote: f.quote}
	}

	resolveBool := func(name string, f parsedBoolField) BoolFieldResult {
		if f.unknown || mustAskSet[name] {
			mustAskSet[name] = true
			return BoolFieldResult{Unknown: true, Sure: false, Reason: ReasonModelUnknown, Quote: f.quote}
		}
		if !f.valid {
			mustAskSet[name] = true
			return BoolFieldResult{Unknown: true, Sure: false, Reason: f.reason, Quote: f.quote}
		}
		return BoolFieldResult{Value: f.val, Sure: true, Quote: f.quote}
	}

	resolveStr := func(name string, f parsedStrField) StringFieldResult {
		if f.unknown || mustAskSet[name] {
			mustAskSet[name] = true
			return StringFieldResult{Value: "UNCLEAR", Unknown: true, Sure: false, Reason: ReasonModelUnknown, Quote: f.quote}
		}
		if !f.valid {
			mustAskSet[name] = true
			return StringFieldResult{Unknown: true, Sure: false, Reason: f.reason, Quote: f.quote}
		}
		return StringFieldResult{Value: f.val, Sure: true, Quote: f.quote}
	}

	res := CheckResult{
		EmailID:                 emailID,
		EmailSHA256:             emailHash,
		Quantity:                resolveInt("quantity", a.quantity),
		UnitPriceCents:          resolveInt("unit_price_cents", a.unitPriceCents),
		DiscountPercent:         resolveInt("discount_percent", a.discountPercent),
		FreeShipping:            resolveBool("free_shipping", a.freeShipping),
		PaymentDays:             resolveInt("payment_days", a.paymentDays),
		RebatePercent:           resolveInt("rebate_percent", a.rebatePercent),
		PriceIncreaseDiscounted: resolveBool("price_increase_discounted", a.priceIncreaseDiscounted),
		AcceptedOption:          resolveStr("accepted_option", a.acceptedOption),
		Terms:                   dedupeTerms(a.terms),
		BuyerInterest:           a.buyerInterest,
		BothAgreedFields:        map[string]bool{},
		CallsUsed:               callsUsed,
		TokensUsed:              tokensUsed,
		SecondsTaken:            elapsed,
		Mode:                    mode,
	}
	res.MustAsk = orderedMustAsk(mustAskSet)
	return res
}

func combineTwoReadings(emailID, emailHash string, a, b verifiedReading, callsUsed, tokensUsed int, elapsed float64, mode string) CheckResult {
	mustAskSet := make(map[string]bool)
	for k, v := range a.mustAsk {
		if v {
			mustAskSet[k] = true
		}
	}
	for k, v := range b.mustAsk {
		if v {
			mustAskSet[k] = true
		}
	}
	bothAgreed := make(map[string]bool)

	combineInt := func(name string, fa, fb parsedIntField) IntFieldResult {
		if fa.unknown || fb.unknown || mustAskSet[name] {
			mustAskSet[name] = true
			q := fa.quote
			if q == "" {
				q = fb.quote
			}
			return IntFieldResult{Unknown: true, Sure: false, Reason: ReasonModelUnknown, Quote: q}
		}
		if !fa.valid {
			mustAskSet[name] = true
			return IntFieldResult{Unknown: true, Sure: false, Reason: fa.reason, Quote: fa.quote}
		}
		if !fb.valid {
			mustAskSet[name] = true
			return IntFieldResult{Unknown: true, Sure: false, Reason: fb.reason, Quote: fb.quote}
		}
		if fa.val != fb.val {
			mustAskSet[name] = true
			return IntFieldResult{Unknown: true, Sure: false, Reason: ReasonReadingsDisagree, Quote: fa.quote}
		}
		bothAgreed[name] = true
		return IntFieldResult{Value: fa.val, Sure: true, Quote: fa.quote}
	}

	combineBool := func(name string, fa, fb parsedBoolField) BoolFieldResult {
		if fa.unknown || fb.unknown || mustAskSet[name] {
			mustAskSet[name] = true
			q := fa.quote
			if q == "" {
				q = fb.quote
			}
			return BoolFieldResult{Unknown: true, Sure: false, Reason: ReasonModelUnknown, Quote: q}
		}
		if !fa.valid {
			mustAskSet[name] = true
			return BoolFieldResult{Unknown: true, Sure: false, Reason: fa.reason, Quote: fa.quote}
		}
		if !fb.valid {
			mustAskSet[name] = true
			return BoolFieldResult{Unknown: true, Sure: false, Reason: fb.reason, Quote: fb.quote}
		}
		if fa.val != fb.val {
			mustAskSet[name] = true
			return BoolFieldResult{Unknown: true, Sure: false, Reason: ReasonReadingsDisagree, Quote: fa.quote}
		}
		bothAgreed[name] = true
		return BoolFieldResult{Value: fa.val, Sure: true, Quote: fa.quote}
	}

	combineStr := func(name string, fa, fb parsedStrField) StringFieldResult {
		if fa.unknown || fb.unknown || mustAskSet[name] {
			mustAskSet[name] = true
			q := fa.quote
			if q == "" {
				q = fb.quote
			}
			return StringFieldResult{Value: "UNCLEAR", Unknown: true, Sure: false, Reason: ReasonModelUnknown, Quote: q}
		}
		if !fa.valid {
			mustAskSet[name] = true
			return StringFieldResult{Unknown: true, Sure: false, Reason: fa.reason, Quote: fa.quote}
		}
		if !fb.valid {
			mustAskSet[name] = true
			return StringFieldResult{Unknown: true, Sure: false, Reason: fb.reason, Quote: fb.quote}
		}
		if fa.val != fb.val {
			mustAskSet[name] = true
			return StringFieldResult{Value: "UNCLEAR", Unknown: true, Sure: false, Reason: ReasonReadingsDisagree, Quote: fa.quote}
		}
		bothAgreed[name] = true
		return StringFieldResult{Value: fa.val, Sure: true, Quote: fa.quote}
	}

	res := CheckResult{
		EmailID:                 emailID,
		EmailSHA256:             emailHash,
		Quantity:                combineInt("quantity", a.quantity, b.quantity),
		UnitPriceCents:          combineInt("unit_price_cents", a.unitPriceCents, b.unitPriceCents),
		DiscountPercent:         combineInt("discount_percent", a.discountPercent, b.discountPercent),
		FreeShipping:            combineBool("free_shipping", a.freeShipping, b.freeShipping),
		PaymentDays:             combineInt("payment_days", a.paymentDays, b.paymentDays),
		RebatePercent:           combineInt("rebate_percent", a.rebatePercent, b.rebatePercent),
		PriceIncreaseDiscounted: combineBool("price_increase_discounted", a.priceIncreaseDiscounted, b.priceIncreaseDiscounted),
		AcceptedOption:          combineStr("accepted_option", a.acceptedOption, b.acceptedOption),
		Terms:                   mergeTwoModelTerms(a.terms, b.terms),
		BuyerInterest:           a.buyerInterest,
		BothAgreedFields:        bothAgreed,
		CallsUsed:               callsUsed,
		TokensUsed:              tokensUsed,
		SecondsTaken:            elapsed,
		Mode:                    mode,
	}
	res.MustAsk = orderedMustAsk(mustAskSet)
	return res
}

func termKey(t TermResult) string {
	return fmt.Sprintf("%s|%v|%s|%s", t.Name, t.Value, t.By, t.Status)
}

func dedupeTerms(terms []TermResult) []TermResult {
	seen := make(map[string]bool)
	var out []TermResult
	for _, t := range terms {
		k := termKey(t)
		if !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
}

func mergeTwoModelTerms(a, b []TermResult) []TermResult {
	// Keep verified terms agreed by both models, or if one model produced terms, dedupe from A.
	bSet := make(map[string]bool)
	for _, t := range b {
		bSet[termKey(t)] = true
	}
	var agreed []TermResult
	seen := make(map[string]bool)
	for _, t := range a {
		k := termKey(t)
		if bSet[k] && !seen[k] {
			seen[k] = true
			agreed = append(agreed, t)
		}
	}
	if len(agreed) > 0 {
		return agreed
	}
	return dedupeTerms(a)
}

var canonicalFieldOrder = []string{
	"quantity",
	"unit_price_cents",
	"discount_percent",
	"free_shipping",
	"payment_days",
	"rebate_percent",
	"price_increase_discounted",
	"accepted_option",
	"currency",
}

func orderedMustAsk(set map[string]bool) []string {
	var out []string
	seen := make(map[string]bool)
	for _, f := range canonicalFieldOrder {
		if set[f] {
			out = append(out, f)
			seen[f] = true
		}
	}
	for f, v := range set {
		if v && !seen[f] {
			out = append(out, f)
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func appendUnique(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}

func writeTraceFile(traceDir, emailID, emailHash, promptVer string, models []string, trace TraceFile) (string, error) {
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		return "", err
	}
	idPart := emailID
	if idPart == "" {
		idPart = emailHash[:12]
	}
	modelPart := strings.Join(models, "_")
	modelPart = strings.ReplaceAll(modelPart, "/", "-")
	fileName := fmt.Sprintf("trace-%s-%s-%s-%s.json", idPart, promptVer, modelPart, emailHash[:8])
	fullPath := filepath.Join(traceDir, fileName)
	data, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(fullPath, data, 0o644); err != nil {
		return "", err
	}
	return fullPath, nil
}
