// Command actcheck runs the ACT path against the PayPal sandbox, without the screen: the quantity choice (one
// call) and the early-payment choice (two calls) are planned, approved and executed, then read back; a repeat
// must create nothing new; a plan changed after approval and a tampered approved plan must both be refused.
// It prints PASS or FAIL per step, with no secrets, no emails and only the last 4 characters of ids.
// Drafts only: nothing is ever sent.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"margincheck/internal/act"
	"margincheck/internal/offer"
	"margincheck/internal/paypal"
	"margincheck/internal/profit"
)

func main() {
	if err := run(context.Background(), os.Stdout, ".env", "testdata/seller.json"); err != nil {
		fmt.Fprintln(os.Stdout, "STOP:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, out io.Writer, dotenv, sellerPath string) error {
	cfg, ready, err := paypal.LoadSandboxConfig(dotenv)
	if err != nil {
		return err
	}
	if !ready {
		fmt.Fprintln(out, paypal.MissingEnvMessage)
		return errors.New("no .env")
	}
	client, err := paypal.NewClient(cfg.APIBase, cfg.ClientID, cfg.ClientSecret, nil)
	if err != nil {
		return err
	}
	seller, err := profit.LoadSeller(sellerPath)
	if err != nil {
		return err
	}
	set, err := offer.Build(offer.Request{Currency: "USD", Quantity: 20, UnitPriceCents: 4000, RequestedPaymentDays: 60}, seller, offer.Config{})
	if err != nil {
		return err
	}
	parties := act.Parties{SellerEmail: cfg.SellerEmail, BuyerEmail: cfg.BuyerEmail, ItemName: "Stoneware mugs (box of 6)"}
	tools := act.PayPalTools{Client: client, Secrets: []string{cfg.SellerEmail, cfg.BuyerEmail}}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	dealID := "k3-" + time.Now().UTC().Format("0102150405")
	fmt.Fprintf(out, "deal %s, invoice date %s, request: 20 boxes at $40.00, 60 days asked\n", dealID, day.Format("2006-01-02"))

	failed := false
	step := func(name string, ok bool, detail string) {
		mark := "PASS"
		if !ok {
			mark, failed = "FAIL", true
		}
		fmt.Fprintf(out, "%s %s: %s\n", mark, name, detail)
	}

	var early offer.Option
	var earlyPlan act.Plan
	var earlyID string
	for _, o := range set.Options {
		if o.Kind == offer.Standard {
			continue
		}
		plan := act.ExpectedPlan(dealID, o, parties, day)
		appr := act.Approve(plan)
		res, err := act.Execute(ctx, tools, appr, plan, o, parties, day)
		detail := fmt.Sprintf("%d call(s), fingerprint ...%s, draft %s, status %s, due date set %v; read-back: %s",
			len(plan.Calls), appr.Fingerprint[len(appr.Fingerprint)-8:], paypal.MaskID(res.InvoiceID), res.Status, res.DueDateSet, checks(res))
		if err != nil {
			detail += "; error: " + scrub(err.Error(), cfg.SellerEmail, cfg.BuyerEmail)
		}
		step(fmt.Sprintf("execute %s (%s)", o.Kind, o.Summary), err == nil && res.ReadBackOK, detail)
		if o.Kind == offer.EarlyPayment {
			early, earlyPlan, earlyID = o, plan, res.InvoiceID
		}
	}
	if earlyID == "" {
		return errors.New("the early-payment run did not create a draft; stopping")
	}

	again, err := act.Execute(ctx, tools, act.Approve(earlyPlan), earlyPlan, early, parties, day)
	step("repeat early_payment", err == nil && again.AlreadyExisted && again.InvoiceID == earlyID,
		fmt.Sprintf("draft %s, already existed %v, same draft %v", paypal.MaskID(again.InvoiceID), again.AlreadyExisted, again.InvoiceID == earlyID))

	changed := act.ExpectedPlan(dealID, early, parties, day)
	appr := act.Approve(changed)
	changed.Calls[0].Create.Quantity = 200
	_, err = act.Execute(ctx, tools, appr, changed, early, parties, day)
	step("plan changed after approval", errors.Is(err, act.ErrFingerprintMismatch), fmt.Sprint(err))

	tampered := act.ExpectedPlan(dealID, early, parties, day)
	tampered.Calls[0].Create.DiscountBP = 5000
	_, err = act.Execute(ctx, tools, act.Approve(tampered), tampered, early, parties, day)
	step("tampered plan, even when approved", errors.Is(err, act.ErrBlocked), firstLine(err))

	if failed {
		return errors.New("a step failed")
	}
	fmt.Fprintln(out, "ALL PASS (nothing was sent)")
	return nil
}

func checks(r act.Result) string {
	var parts []string
	for _, c := range r.Checks {
		mark := "ok"
		if !c.OK {
			mark = "MISMATCH want " + c.Want
		}
		if c.Field == "invoice_number" {
			parts = append(parts, fmt.Sprintf("%s=%s %s", c.Field, paypal.MaskID(c.Got), mark))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s %s", c.Field, c.Got, mark))
	}
	return strings.Join(parts, ", ")
}

func firstLine(err error) string {
	if err == nil {
		return "<nil>"
	}
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

func scrub(s string, secrets ...string) string {
	for _, x := range secrets {
		if x != "" {
			s = strings.ReplaceAll(s, x, "[redacted]")
		}
	}
	return s
}
