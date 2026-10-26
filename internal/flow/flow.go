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
	"margincheck/internal/profit"
	"margincheck/internal/reader"
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

	BuyerReply    string              `json:"buyer_reply,omitempty"`
	ReplyReading  *reader.CheckResult `json:"reply_reading,omitempty"`
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
	return deal, d.Store.Save(deal)
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
	res, err := d.Reader.Check(ctx, dealID+"-reply", replyThread)
	if err != nil {
		return nil, err
	}
	deal.BuyerReply, deal.ReplyReading = replyThread, &res
	deal.Accepted, deal.AcceptedQuote, deal.Proposal, deal.Approval, deal.Result, deal.ResultError = "", res.AcceptedOption.Quote, nil, nil, nil, ""

	kind := offer.Kind(strings.ToLower(strings.TrimSpace(res.AcceptedOption.Value)))
	switch {
	case kind == "changed":
		deal.Next, deal.NextReason = NextSellerDecides, "The buyer accepted but changed the terms, so it isn't one of your choices. You decide; nothing is planned."
	case !res.AcceptedOption.Sure || res.AcceptedOption.Unknown || (kind != offer.Standard && kind != offer.Quantity && kind != offer.EarlyPayment):
		deal.Next, deal.NextReason = NextAsk, "It isn't clear which choice the buyer accepted. Ask them; nothing is planned until it is."
	default:
		opt, ok := deal.Option(kind)
		switch {
		case !ok:
			deal.Next, deal.NextReason = NextSellerDecides, "The buyer named a choice you didn't offer. You decide; nothing is planned."
		case statedChange(res, opt, d.Offer) != "":
			deal.Next, deal.NextReason = NextSellerDecides, fmt.Sprintf("The buyer accepted the %s choice but wrote %s. You decide; nothing is planned.",
				writer.Label(kind), statedChange(res, opt, d.Offer))
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
	return deal, d.Store.Save(deal)
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
	if err := d.Store.Save(deal); err != nil {
		return deal, err
	}
	return deal, runErr
}

// statedChange describes the first term the buyer stated that differs from the choice, or returns "".
// A term counts as stated when the reading is sure and quotes the buyer's sentence. An unstated term is fixed
// by the choice itself, and the seller sees the plan before approving it.
func statedChange(res reader.CheckResult, o offer.Option, c offer.Config) string {
	statedInt := func(f reader.IntFieldResult) bool { return f.Sure && !f.Unknown && strings.TrimSpace(f.Quote) != "" }
	switch {
	case statedInt(res.Quantity) && res.Quantity.Value != o.Quantity:
		return fmt.Sprintf("%d %s instead of %d", res.Quantity.Value, unitNoun(c), o.Quantity)
	case statedInt(res.UnitPriceCents) && res.UnitPriceCents.Value != o.UnitPriceCents:
		return fmt.Sprintf("%s a box instead of %s", offer.FormatMoney(res.UnitPriceCents.Value, o.Currency), offer.FormatMoney(o.UnitPriceCents, o.Currency))
	case statedInt(res.PaymentDays) && res.PaymentDays.Value != o.DueDays:
		return fmt.Sprintf("payment in %d days instead of %s", res.PaymentDays.Value, dueWords(o.DueDays))
	case statedInt(res.DiscountPercent) && res.DiscountPercent.Value*100 != o.DiscountBP:
		return fmt.Sprintf("%s off instead of %s", offer.FormatPercent(res.DiscountPercent.Value*100), offer.FormatPercent(o.DiscountBP))
	case res.FreeShipping.Sure && !res.FreeShipping.Unknown && strings.TrimSpace(res.FreeShipping.Quote) != "" && res.FreeShipping.Value != o.FreeShipping:
		if res.FreeShipping.Value {
			return "free shipping, which this choice doesn't include"
		}
		return "that they pay the shipping, while this choice ships free"
	}
	return ""
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
