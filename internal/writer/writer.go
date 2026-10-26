package writer

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"margincheck/internal/gemini"
	"margincheck/internal/offer"
)

// PromptVersion names the instructions below; bump it whenever they change (S5).
const PromptVersion = "writer-v2"

const systemInstruction = `You write the reply a small seller sends to a buyer who asked for a better deal.

Rules:
- Open with one short, warm greeting line, with no personal name (for example, thanking the buyer for reaching out).
- The seller's choices are given as blanks such as {{OPTION_1}}. Give the choices after the greeting line. Put every blank in the reply exactly once, each on its own line. Code replaces each blank with the exact terms.
- Never write a number, in digits or in words, and never a price, a currency sign, a currency name or a percent sign. All numbers come from the blanks.
- Be warm about the buyer and firm about the terms. Do not apologise for the prices and do not offer anything that is not in a blank.
- Never suggest meeting in the middle or splitting the difference.
- You may restate a choice's reason in your own words.
- End with one open question that starts with "How" or "What".
- At most one hundred and twenty words. Plain text, no greeting name, no signature.

Return JSON: {"reply": "<the reply>"}.`

var responseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"reply": map[string]any{"type": "string"},
	},
	"required": []any{"reply"},
}

// Generator is the model client (internal/gemini satisfies it).
type Generator interface {
	Generate(ctx context.Context, req gemini.Request) (gemini.Response, error)
}

// Config configures one Write call.
type Config struct {
	Client   Generator
	Model    string
	MaxWords int // default 160
}

// Draft is the reply shown to the seller, who edits it and sends it herself.
type Draft struct {
	Text     string   `json:"text"`
	ByCode   bool     `json:"by_code"` // true when the code template was used instead of the AI's text
	Attempts int      `json:"attempts"`
	Problems []string `json:"problems,omitempty"` // why AI attempts were rejected
	Model    string   `json:"model,omitempty"`
	Tokens   int      `json:"tokens"`
	Mode     string   `json:"mode,omitempty"` // live or replayed
}

var (
	blankRe  = regexp.MustCompile(`\{\{OPTION_(\d+)\}\}`)
	anyBlank = regexp.MustCompile(`\{\{[^}]*\}\}`)
	banned   = []string{"split the difference", "meet in the middle", "meet halfway", "meet you halfway", "halfway"}
	// Numbers written as words. "one" is left out because it is everyday English ("one more thing").
	numberWordRe = regexp.MustCompile(`(?i)\b(two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety|hundred|thousand|dozen|half|percent|dollars?|euros?|cents?)\b`)
	reThreeNewlines = regexp.MustCompile(`\n{3,}`)
)

// Write asks the AI for a reply around the blanks, checks it, retries once with the reasons, and falls back to
// the code template if it still fails. It never returns an error: the seller always gets a reply to edit.
func Write(ctx context.Context, cfg Config, set offer.Set) Draft {
	if cfg.MaxWords <= 0 {
		cfg.MaxWords = 160
	}
	n := len(set.Options)
	d := Draft{Model: cfg.Model}
	if n == 0 || cfg.Client == nil {
		d.Text, d.ByCode = Template(set), true
		return d
	}

	type choice struct {
		Blank  string `json:"blank"`
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	}
	choices := make([]choice, n)
	for i, o := range set.Options {
		choices[i] = choice{Blank: fmt.Sprintf("{{OPTION_%d}}", i+1), Kind: string(o.Kind), Reason: o.Reason}
	}
	payload, _ := json.Marshal(map[string]any{"choices": choices})
	user := "The seller's choices:\n" + string(payload)

	for attempt := 1; attempt <= 2; attempt++ {
		d.Attempts = attempt
		resp, err := cfg.Client.Generate(ctx, gemini.Request{
			Model: cfg.Model, InstructionVersion: PromptVersion, SystemInstruction: systemInstruction,
			UserText: user, ResponseSchema: responseSchema, MaxOutputTokens: 4096,
		})
		if err != nil {
			d.Problems = append(d.Problems, "model unavailable: "+err.Error())
			break
		}
		d.Tokens += resp.TotalTokens
		d.Mode = resp.Source
		var out struct {
			Reply string `json:"reply"`
		}
		if err := json.Unmarshal([]byte(resp.Text), &out); err != nil {
			d.Problems = append(d.Problems, "the reply was not the requested JSON")
			user += "\n\nYour last answer was not the requested JSON. Return {\"reply\": \"...\"}."
			continue
		}
		probs := Validate(out.Reply, n)
		if len(probs) == 0 {
			d.Text = Fill(out.Reply, set)
			return d
		}
		d.Problems = append(d.Problems, probs...)
		user += "\n\nYour last reply was rejected: " + strings.Join(probs, "; ") + ". Write it again and fix exactly these problems."
	}
	d.Text, d.ByCode = Template(set), true
	return d
}

// Validate lists every reason the AI's reply can't be used. n is the number of choices.
func Validate(text string, n int) []string {
	var probs []string
	if strings.TrimSpace(text) == "" {
		return []string{"the reply is empty"}
	}
	idx := strings.Index(text, "{{OPTION_")
	wordsWithLetters := 0
	if idx >= 0 {
		prefix := strings.TrimSpace(text[:idx])
		if !strings.HasPrefix(strings.ToLower(prefix), "option") {
			for _, w := range strings.Fields(prefix) {
				if strings.IndexFunc(w, unicode.IsLetter) >= 0 {
					wordsWithLetters++
				}
			}
		}
	}
	if wordsWithLetters < 2 {
		probs = append(probs, "the reply must open with a greeting before the choices")
	}
	counts := map[int]int{}
	for _, m := range blankRe.FindAllStringSubmatch(text, -1) {
		i, _ := strconv.Atoi(m[1])
		counts[i]++
	}
	for i := 1; i <= n; i++ {
		switch c := counts[i]; {
		case c == 0:
			probs = append(probs, fmt.Sprintf("blank {{OPTION_%d}} is missing", i))
		case c > 1:
			probs = append(probs, fmt.Sprintf("blank {{OPTION_%d}} appears more than once", i))
		}
	}
	for _, b := range anyBlank.FindAllString(text, -1) {
		m := blankRe.FindStringSubmatch(b)
		if m == nil {
			probs = append(probs, "unknown blank "+b)
			continue
		}
		if i, _ := strconv.Atoi(m[1]); i < 1 || i > n {
			probs = append(probs, "unknown blank "+b)
		}
	}
	rest := anyBlank.ReplaceAllString(text, "")
	if HasNumber(rest) {
		probs = append(probs, "it contains a digit; numbers come only from the blanks")
	}
	if HasCurrencySign(rest) {
		probs = append(probs, "it contains a currency sign")
	}
	if HasPercentSign(rest) {
		probs = append(probs, "it contains a percent sign")
	}
	if w := numberWordRe.FindString(rest); w != "" {
		probs = append(probs, fmt.Sprintf("it contains the number word %q; numbers come only from the blanks", w))
	}
	low := strings.ToLower(rest)
	for _, b := range banned {
		if strings.Contains(low, b) {
			probs = append(probs, fmt.Sprintf("it suggests splitting the difference (%q)", b))
			break
		}
	}
	if w := len(strings.Fields(rest)); w > 160 {
		probs = append(probs, fmt.Sprintf("it has %d words; the limit is 160", w))
	}
	return probs
}

// HasNumber reports whether s contains any character Unicode counts as a number: 0-9, but also full-width
// digits, other scripts' digits, fractions such as ½ and superscripts. A fixed list of "bad" characters misses these.
func HasNumber(s string) bool { return strings.IndexFunc(s, unicode.IsNumber) >= 0 }

// HasCurrencySign reports whether s contains any currency sign ($, €, £, ¥, ₾, ₹, …).
func HasCurrencySign(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Sc, r) }) >= 0
}

// HasPercentSign reports whether s contains a percent sign in any of its forms.
func HasPercentSign(s string) bool { return strings.ContainsAny(s, "%％﹪‰‱") }

// Fill replaces each blank with its choice's terms, written by code, and lays out the reply.
func Fill(text string, set offer.Set) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	locs := blankRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return text
	}

	greeting := strings.TrimSpace(text[:locs[0][0]])
	closing := strings.TrimSpace(text[locs[len(locs)-1][1]:])

	choices := make([]string, len(locs))
	for idx, loc := range locs {
		b := text[loc[0]:loc[1]]
		m := blankRe.FindStringSubmatch(b)
		i, _ := strconv.Atoi(m[1])
		if i >= 1 && i <= len(set.Options) {
			choices[idx] = fmt.Sprintf("Option %d (%s): %s.", i, Label(set.Options[i-1].Kind), set.Options[i-1].Summary)
		} else {
			choices[idx] = b
		}
	}

	var b strings.Builder
	if greeting != "" {
		b.WriteString(greeting)
		b.WriteString("\n\n")
	}
	b.WriteString(strings.Join(choices, "\n"))
	if closing != "" {
		b.WriteString("\n\n")
		b.WriteString(closing)
	}

	out := b.String()
	out = reThreeNewlines.ReplaceAllString(out, "\n\n")
	return out
}

// Template is the reply written entirely by code, used when the AI's reply can't be used.
func Template(set offer.Set) string {
	var b strings.Builder
	b.WriteString("Thank you for the order. Here is what we can do:\n\n")
	for i, o := range set.Options {
		fmt.Fprintf(&b, "Option %d (%s): %s. %s\n", i+1, Label(o.Kind), o.Summary, o.Reason)
	}
	b.WriteString("\nWhat works best for you?")
	return b.String()
}

// Label names a choice the way the reader expects to find it in the buyer's reply.
func Label(k offer.Kind) string {
	switch k {
	case offer.Standard:
		return "Standard"
	case offer.Quantity:
		return "Quantity"
	case offer.EarlyPayment:
		return "Early payment"
	}
	return string(k)
}
