package flow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"margincheck/internal/act"
	"margincheck/internal/check"
	"margincheck/internal/offer"
	"margincheck/internal/paypal"
	"margincheck/internal/profit"
	"margincheck/internal/reader"
	"margincheck/internal/why"
	"margincheck/internal/writer"
)

// What happens after the buyer answers.
const (
	NextDraft         = "draft"          // a choice was accepted: a plan waits for the seller's approval
	NextAsk           = "ask"            // the answer is unclear: the seller asks the buyer
	NextSellerDecides = "seller_decides" // the buyer changed the terms: the seller decides, nothing is planned
)

// RunTimeLimit bounds one approved run. Once started, a run is not stopped by the browser closing the request
// (that could leave the draft made but its due date unset); it stops only at this limit.
const RunTimeLimit = 3 * time.Minute

// Reader reads one email thread (internal/reader satisfies it).
type Reader interface {
	Check(ctx context.Context, emailID, rawEmail string) (reader.CheckResult, error)
}

// Deps are everything the flow uses. The seller's numbers and the parties come from settings, never the email.
type Deps struct {
	Reader       Reader
	Writer       writer.Generator
	WriterModel  string
	Planner      act.Generator
	PlannerModel string
	Tools        act.Tools
	Seller       profit.Seller
	Parties      act.Parties
	Offer        offer.Config
	Store        *Store
	Now          func() time.Time
}

// Deal is one negotiation, from the pasted thread to the PayPal draft.
type Deal struct {
	ID         string             `json:"id"`
	Created    time.Time          `json:"created"`
	InvoiceDay string             `json:"invoice_day"`
	Thread     string             `json:"thread"`
	Reading    reader.CheckResult `json:"reading"`
	Check      check.Result       `json:"check"`
	Request    *offer.Request     `json:"request,omitempty"`
	Offers     *offer.Set         `json:"offers,omitempty"`
	OffersNote string             `json:"offers_note,omitempty"`
	Reply      *writer.Draft      `json:"reply,omitempty"`
	Why        *why.Note          `json:"why,omitempty"` // the AI's guess why the buyer is asking; for the seller only

	BuyerReply    string              `json:"buyer_reply,omitempty"`
	ReplyReading  *reader.CheckResult `json:"reply_reading,omitempty"`
	AIPick        offer.Kind          `json:"ai_pick,omitempty"` // the choice the AI read in the buyer's own words
	Accepted      offer.Kind          `json:"accepted,omitempty"`
	AcceptedQuote string              `json:"accepted_quote,omitempty"`
	Next          string              `json:"next,omitempty"`
	NextReason    string              `json:"next_reason,omitempty"`
	Proposal      *act.Proposal       `json:"proposal,omitempty"`

	Approval    *act.Approval `json:"approval,omitempty"`
	Result      *act.Result   `json:"result,omitempty"`
	ResultError string        `json:"result_error,omitempty"`
}

// Option returns the stored choice of the given kind.
func (d *Deal) Option(k offer.Kind) (offer.Option, bool) {
	if d.Offers == nil {
		return offer.Option{}, false
	}
	for _, o := range d.Offers.Options {
		if o.Kind == k {
			return o, true
		}
	}
	return offer.Option{}, false
}

// invoiceDay is the day the deal's draft is dated. A damaged value stops the deal instead of becoming year 1.
func (d *Deal) invoiceDay() (time.Time, error) {
	day, err := time.Parse("2006-01-02", d.InvoiceDay)
	if err != nil {
		return time.Time{}, fmt.Errorf("this deal's date (%q) is damaged; start the check again", d.InvoiceDay)
	}
	return day, nil
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// Start reads a pasted thread, checks what the deal keeps, builds the choices and drafts the reply.
func (d Deps) Start(ctx context.Context, thread string) (*Deal, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := d.now()
	deal := &Deal{ID: id, Created: now, InvoiceDay: now.Format("2006-01-02"), Thread: thread}

	res, err := d.Reader.Check(ctx, id, thread)
	if err != nil {
		return nil, err
	}
	deal.Reading = res
	deal.Check = check.CheckEmail(res, d.Seller, reader.StripMetadataLine(thread))

	// The choices need a sure quantity, a sure price and one currency; never a guess.
	cur, okCur := check.DealCurrency(res)
	switch {
	case !res.Quantity.Sure || res.Quantity.Unknown || !res.UnitPriceCents.Sure || res.UnitPriceCents.Unknown:
		deal.OffersNote = "No choices yet: the quantity and the price must both be sure first."
	case !okCur:
		deal.OffersNote = "No choices yet: the deal's currency is unclear."
	default:
		req := offer.Request{
			Currency: cur, Quantity: res.Quantity.Value, UnitPriceCents: res.UnitPriceCents.Value,
			RequestedPaymentDays: requestedInt(res, "payment_days", 1),
			RequestedDiscountBP:  requestedInt(res, "discount_percent", 100),
		}
		// When the thread shows the payment terms already on the table (a sure, quoted reading), the choices answer
		// those terms, not the buyer's earlier ask: if the seller held firm on receipt, a discount for paying in
		// 7 days would keep less than the deal already agreed.
		if res.PaymentDays.Sure && !res.PaymentDays.Unknown && strings.TrimSpace(res.PaymentDays.Quote) != "" {
			req.RequestedPaymentDays, req.PaymentDaysSettled = res.PaymentDays.Value, true
		}
		deal.Request = &req
		set, err := offer.Build(req, d.Seller, d.Offer)
		if err != nil {
			deal.OffersNote = "No choices: " + err.Error()
		} else {
			deal.Offers = &set
			draft := writer.Write(ctx, writer.Config{Client: d.Writer, Model: d.WriterModel}, set)
			deal.Reply = &draft
		}
	}
	// A separate call, after the reply is written: the guess is for the seller only, and the writer never sees it.
	if d.Writer != nil {
		note := why.Read(ctx, why.Config{Client: d.Writer, Model: d.WriterModel}, thread)
		deal.Why = &note
	}
	if err := d.Store.Save(deal); err != nil {
		return deal, err
	}
	if err := d.Store.record(deal.ID, "read", fmt.Sprintf("thread read; outcome %s", deal.Check.Outcome)); err != nil {
		return deal, err
	}
	if err := d.Store.record(deal.ID, "choices", choicesLine(deal)); err != nil {
		return deal, err
	}
	if deal.Why == nil {
		return deal, nil
	}
	return deal, d.Store.record(deal.ID, "why", fmt.Sprintf("%d guess(es) shown to the seller; %d problem(s)", len(deal.Why.Guesses), len(deal.Why.Problems)))
}

func choicesLine(deal *Deal) string {
	if deal.Offers == nil {
		return deal.OffersNote
	}
	var kinds []string
	for _, o := range deal.Offers.Options {
		kinds = append(kinds, fmt.Sprintf("%s keeps %d cents", o.Kind, o.Profit.ProfitCents))
	}
	line := "offered: " + strings.Join(kinds, "; ")
	if len(deal.Offers.Omitted) > 0 {
		line += "; left out: " + strings.Join(deal.Offers.Omitted, "; ")
	}
	return line
}

// requestedInt returns the largest value of a buyer's requested term, times scale (0 if none).
func requestedInt(res reader.CheckResult, name string, scale int64) int64 {
	var best int64
	for _, t := range res.Terms {
		if t.Name != name || !strings.EqualFold(t.By, "buyer") || !strings.EqualFold(t.Status, "requested") {
			continue
		}
		var v int64
		switch x := t.Value.(type) {
		case float64:
			v = int64(x)
		case int:
			v = int64(x)
		case int64:
			v = x
		case json.Number:
			v, _ = x.Int64()
		default:
			continue
		}
		if v*scale > best {
			best = v * scale
		}
	}
	return best
}

// BuyerReplied reads the buyer's answer. Only a sure, offered, unchanged choice leads to a plan (M8).
func (d Deps) BuyerReplied(ctx context.Context, dealID, replyThread string) (*Deal, error) {
	defer d.Store.lock(dealID)()
	deal, err := d.Store.Load(dealID)
	if err != nil {
		return nil, err
	}
	if deal.Offers == nil {
		return nil, errors.New("this deal has no choices to accept yet")
	}
	// A short answer ("Option 2 please") only makes sense next to the choices we sent, so the reader gets them,
	// written by the app, before the buyer's words, the way an email shows the earlier message. What proves
	// acceptance, or a changed term, must still be the buyer's own words (fromBuyer).
	readText := replyThread
	if !strings.Contains(replyThread, "Option 1 (") {
		readText = choicesContext(deal.Offers) + "\n" + replyThread
	}
	fromBuyer := func(quote string) bool {
		q := flatten(quote)
		return q != "" && strings.Contains(flatten(replyThread), q)
	}
	res, err := d.Reader.Check(ctx, dealID+"-reply", readText)
	if err != nil {
		return nil, err
	}
	deal.BuyerReply, deal.ReplyReading = replyThread, &res
	deal.Accepted, deal.AcceptedQuote, deal.Proposal, deal.Approval, deal.Result, deal.ResultError = "", res.AcceptedOption.Quote, nil, nil, nil, ""

	kind := offer.Kind(strings.ToLower(strings.TrimSpace(res.AcceptedOption.Value)))
	deal.AIPick = ""
	if fromBuyer(res.AcceptedOption.Quote) && res.AcceptedOption.Sure && !res.AcceptedOption.Unknown {
		if _, offered := deal.Option(kind); offered {
			deal.AIPick = kind // shown to the seller, who confirms or corrects it with one tap
		}
	}
	switch {
	case !fromBuyer(res.AcceptedOption.Quote):
		deal.Next, deal.NextReason = NextAsk, "It isn't clear which choice the buyer accepted. Ask them; nothing is planned until it is."
	case kind == "changed":
		deal.Next, deal.NextReason = NextSellerDecides, "The buyer accepted but changed the terms, so it isn't one of your choices. You decide; nothing is planned."
	case !res.AcceptedOption.Sure || res.AcceptedOption.Unknown || (kind != offer.Standard && kind != offer.Quantity && kind != offer.EarlyPayment):
		deal.Next, deal.NextReason = NextAsk, "It isn't clear which choice the buyer accepted. Ask them; nothing is planned until it is."
	default:
		opt, ok := deal.Option(kind)
		switch {
		case !ok:
			deal.Next, deal.NextReason = NextSellerDecides, "The buyer named a choice you didn't offer. You decide; nothing is planned."
		case statedChange(res, opt, d.Offer, fromBuyer) != "":
			deal.Next, deal.NextReason = NextSellerDecides, fmt.Sprintf("The buyer accepted the %s choice but wrote %s. You decide; nothing is planned.",
				writer.Label(kind), statedChange(res, opt, d.Offer, fromBuyer))
		default:
			day, err := deal.invoiceDay()
			if err != nil {
				return nil, err
			}
			deal.Accepted = kind
			deal.Next = NextDraft
			deal.NextReason = fmt.Sprintf("The buyer accepted the %s choice.", writer.Label(kind))
			pr := act.Propose(ctx, d.Planner, d.PlannerModel, deal.ID, opt, d.Parties, day)
			deal.Proposal = &pr
		}
	}
	if err := d.Store.Save(deal); err != nil {
		return deal, err
	}
	if err := d.Store.record(deal.ID, "reply", fmt.Sprintf("next %s: %s", deal.Next, deal.NextReason)); err != nil {
		return deal, err
	}
	if deal.Proposal != nil {
		p := deal.Proposal
		return deal, d.Store.record(deal.ID, "plan", fmt.Sprintf("by %s, %d call(s), fingerprint %s, AI plan blocked for %d difference(s)",
			p.Plan.By, len(p.Plan.Calls), act.Fingerprint(p.Plan), len(p.Blocked)))
	}
	return deal, nil
}

// SellerChose records which choice the buyer accepted, as the seller confirms or corrects it with one tap after
// reading the buyer's email. The same rules apply as when the AI reads it: a term the buyer stated differently
// still goes to the seller to decide, and once a PayPal draft exists the choice can't be switched.
func (d Deps) SellerChose(ctx context.Context, dealID string, kind offer.Kind) (*Deal, error) {
	defer d.Store.lock(dealID)()
	deal, err := d.Store.Load(dealID)
	if err != nil {
		return nil, err
	}
	if deal.Offers == nil || deal.ReplyReading == nil {
		return deal, errors.New("read the buyer's answer first")
	}
	if deal.Result != nil {
		return deal, errors.New("a PayPal draft was already made for this deal, so its choice can't be switched")
	}
	opt, ok := deal.Option(kind)
	if !ok {
		return deal, fmt.Errorf("%q isn't one of the choices you offered", kind)
	}
	fromBuyer := func(quote string) bool {
		q := flatten(quote)
		return q != "" && strings.Contains(flatten(deal.BuyerReply), q)
	}
	deal.Accepted, deal.Proposal, deal.Approval, deal.ResultError = "", nil, nil, ""
	if change := statedChange(*deal.ReplyReading, opt, d.Offer, fromBuyer); change != "" {
		deal.Next, deal.NextReason = NextSellerDecides, fmt.Sprintf("You chose the %s choice, but the buyer wrote %s. You decide; nothing is planned.",
			writer.Label(kind), change)
	} else {
		day, err := deal.invoiceDay()
		if err != nil {
			return deal, err
		}
		deal.Accepted, deal.Next = kind, NextDraft
		deal.NextReason = fmt.Sprintf("You confirmed it: the buyer accepted the %s choice.", writer.Label(kind))
		pr := act.Propose(ctx, d.Planner, d.PlannerModel, deal.ID, opt, d.Parties, day)
		deal.Proposal = &pr
	}
	if err := d.Store.Save(deal); err != nil {
		return deal, err
	}
	if err := d.Store.record(deal.ID, "seller chose", fmt.Sprintf("%s (the AI read %q); next %s", kind, deal.AIPick, deal.Next)); err != nil {
		return deal, err
	}
	if deal.Proposal != nil {
		p := deal.Proposal
		return deal, d.Store.record(deal.ID, "plan", fmt.Sprintf("by %s, %d call(s), fingerprint %s, AI plan blocked for %d difference(s)",
			p.Plan.By, len(p.Plan.Calls), act.Fingerprint(p.Plan), len(p.Blocked)))
	}
	return deal, nil
}

// ApproveAndRun records the seller's approval of the plan they were shown (its fingerprint) and runs it.
// It refuses if the stored plan no longer has that fingerprint. Two approvals of one deal (a double click) run
// one after the other, so the second finds the draft the first made.
func (d Deps) ApproveAndRun(ctx context.Context, dealID, shownFingerprint string) (*Deal, error) {
	defer d.Store.lock(dealID)()
	deal, err := d.Store.Load(dealID)
	if err != nil {
		return nil, err
	}
	if deal.Proposal == nil || deal.Next != NextDraft {
		return deal, errors.New("there is no plan waiting for approval")
	}
	plan := deal.Proposal.Plan
	if act.Fingerprint(plan) != strings.TrimSpace(shownFingerprint) {
		return deal, act.ErrFingerprintMismatch
	}
	// This exact plan already ran and PayPal's draft was read back: a second click changes nothing.
	if deal.Result != nil && deal.Result.ReadBackOK && deal.Approval != nil && deal.Approval.Fingerprint == act.Fingerprint(plan) {
		return deal, nil
	}
	opt, ok := deal.Option(deal.Accepted)
	if !ok {
		return deal, errors.New("the accepted choice is missing from the deal")
	}
	day, err := deal.invoiceDay()
	if err != nil {
		return deal, err
	}
	// The approval is written to the log before PayPal is called: no PayPal action without its record.
	if err := d.Store.record(deal.ID, "approved", "fingerprint "+act.Fingerprint(plan)); err != nil {
		return deal, err
	}
	appr := act.Approve(plan)
	deal.Approval = &appr
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RunTimeLimit)
	defer cancel()
	res, runErr := act.Execute(runCtx, d.Tools, appr, plan, opt, d.Parties, day)
	deal.Result = &res
	deal.ResultError = ""
	if runErr != nil {
		deal.ResultError = runErr.Error()
	}
	if err := d.Store.record(deal.ID, "ran", fmt.Sprintf("draft %s, status %s, read-back ok %v, already existed %v; error: %s",
		paypal.MaskID(res.InvoiceID), res.Status, res.ReadBackOK, res.AlreadyExisted, deal.ResultError)); err != nil && runErr == nil {
		runErr = err
	}
	if err := d.Store.Save(deal); err != nil {
		return deal, err
	}
	return deal, runErr
}

// statedChange describes the first term the buyer stated that differs from the choice, or returns "".
// A term counts as stated when the reading is sure and quotes the buyer's own sentence (fromBuyer). An unstated
// term is fixed by the choice itself, and the seller sees the plan before approving it.
func statedChange(res reader.CheckResult, o offer.Option, c offer.Config, fromBuyer func(string) bool) string {
	statedInt := func(f reader.IntFieldResult) bool { return f.Sure && !f.Unknown && fromBuyer(f.Quote) }
	switch {
	case statedInt(res.Quantity) && res.Quantity.Value != o.Quantity:
		return fmt.Sprintf("%d %s instead of %d", res.Quantity.Value, unitNoun(c), o.Quantity)
	case statedInt(res.UnitPriceCents) && res.UnitPriceCents.Value != o.UnitPriceCents:
		return fmt.Sprintf("%s a box instead of %s", offer.FormatMoney(res.UnitPriceCents.Value, o.Currency), offer.FormatMoney(o.UnitPriceCents, o.Currency))
	case statedInt(res.PaymentDays) && res.PaymentDays.Value != o.DueDays:
		return fmt.Sprintf("payment in %d days instead of %s", res.PaymentDays.Value, dueWords(o.DueDays))
	case statedInt(res.DiscountPercent) && res.DiscountPercent.Value*100 != o.DiscountBP:
		return fmt.Sprintf("%s off instead of %s", offer.FormatPercent(res.DiscountPercent.Value*100), offer.FormatPercent(o.DiscountBP))
	case res.FreeShipping.Sure && !res.FreeShipping.Unknown && fromBuyer(res.FreeShipping.Quote) && res.FreeShipping.Value != o.FreeShipping:
		if res.FreeShipping.Value {
			return "free shipping, which this choice doesn't include"
		}
		return "that they pay the shipping, while this choice ships free"
	}
	return ""
}

// choicesContext is the seller's message with the choices, as the app wrote it into the reply.
func choicesContext(set *offer.Set) string {
	var b strings.Builder
	b.WriteString("Seller:\n")
	for i, o := range set.Options {
		fmt.Fprintf(&b, "> Option %d (%s): %s.\n", i+1, writer.Label(o.Kind), o.Summary)
	}
	return b.String()
}

// flatten makes quote matching ignore line breaks, spacing and email quote marks.
func flatten(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, ">", " ")), " ")
}

func dueWords(days int64) string {
	if days == 0 {
		return "on receipt"
	}
	return fmt.Sprintf("within %d days", days)
}

func unitNoun(c offer.Config) string {
	if strings.TrimSpace(c.UnitNoun) != "" {
		return c.UnitNoun
	}
	return "boxes"
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "d" + hex.EncodeToString(b), nil
}

// Store keeps deals as JSON files in one folder (margin-check/.data/deals, git-ignored).
type Store struct {
	dir string

	mu    sync.Mutex
	deals map[string]*sync.Mutex // one lock per deal: changes to a deal happen one at a time

	failLog bool // tests only: make every log write fail
}

// record appends one line to the deal's decision log (.data/deals/<id>.log). The log only grows: lines are
// never rewritten, so it shows the order in which things happened even after the deal file is updated.
func (s *Store) record(id, step, detail string) error {
	if s.failLog {
		return errors.New("flow: the decision log can't be written; nothing was done")
	}
	if !idRe.MatchString(id) {
		return fmt.Errorf("flow: bad deal id %q", id)
	}
	line, err := json.Marshal(struct {
		At     string `json:"at"`
		Step   string `json:"step"`
		Detail string `json:"detail"`
	}{time.Now().UTC().Format(time.RFC3339), step, emailRe.ReplaceAllString(detail, "***@***")})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, id+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+`)

// Log returns the deal's decision log, one JSON line per step ("" if nothing was recorded yet).
func (s *Store) Log(id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", fmt.Errorf("flow: bad deal id %q", id)
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, id+".log"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(raw), err
}

// lock waits until no other change to this deal is running, and returns the unlock.
func (s *Store) lock(id string) func() {
	s.mu.Lock()
	if s.deals == nil {
		s.deals = map[string]*sync.Mutex{}
	}
	m, ok := s.deals[id]
	if !ok {
		m = &sync.Mutex{}
		s.deals[id] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

var idRe = regexp.MustCompile(`^d[0-9a-f]{12}$`)

// NewStore opens (and creates) the folder.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Save writes a deal atomically.
func (s *Store) Save(d *Deal) error {
	if !idRe.MatchString(d.ID) {
		return fmt.Errorf("flow: bad deal id %q", d.ID)
	}
	raw, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, d.ID+".json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, d.ID+".json"))
}

// Load reads a deal by its id; ids are checked so a request can't reach outside the folder.
func (s *Store) Load(id string) (*Deal, error) {
	if !idRe.MatchString(id) {
		return nil, fmt.Errorf("flow: bad deal id %q", id)
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var d Deal
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}
