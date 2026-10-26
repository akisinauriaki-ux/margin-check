// Command app is the profit spell-checker's screen. It runs on this computer only (127.0.0.1).
//
//	go run ./cmd/app            then open http://127.0.0.1:8787
//
// The seller pastes a thread, sees what the deal keeps and three fair choices, edits the drafted reply and sends
// it herself. When the buyer answers, the agent proposes the matching PayPal draft; nothing runs until the seller
// approves that exact plan. Sandbox only, drafts only: nothing is ever sent.
package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"margincheck/internal/act"
	"margincheck/internal/flow"
	"margincheck/internal/gemini"
	"margincheck/internal/offer"
	"margincheck/internal/paypal"
	"margincheck/internal/profit"
	"margincheck/internal/reader"
)

//go:embed page.html
var pageFS embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address; must stay on this computer")
	models := flag.String("models", "gemini-3.5-flash-lite,gemini-3.1-flash-lite", "the two reading models (decision 0019)")
	helperModel := flag.String("helper-model", "gemini-3.5-flash-lite", "the model that drafts the reply and the plan")
	item := flag.String("item", "Stoneware mugs (box of 6)", "the line-item name on the PayPal draft")
	sellerPath := flag.String("seller", "testdata/seller.json", "the seller's numbers (sample values for the demo)")
	flag.Parse()

	if !strings.HasPrefix(*addr, "127.0.0.1:") && !strings.HasPrefix(*addr, "localhost:") {
		log.Fatal("app: the screen only listens on this computer (127.0.0.1)")
	}
	cfg, ready, err := paypal.LoadSandboxConfig(".env")
	if err != nil {
		log.Fatal(err)
	}
	if !ready {
		log.Fatal(paypal.MissingEnvMessage)
	}
	key, err := gemini.LoadAPIKey(".env")
	if err != nil {
		log.Fatal("app: GEMINI_API_KEY missing from .env")
	}
	gem, err := gemini.NewClient(key, filepath.Join(".cache", "gemini"), false)
	if err != nil {
		log.Fatal(err)
	}
	pair := strings.Split(*models, ",")
	if len(pair) != 2 {
		log.Fatal("app: -models needs two model names")
	}
	rd, err := reader.New(reader.Config{Client: gem, PromptPath: filepath.Join("prompts", "reader-v1.md"),
		ModelA: strings.TrimSpace(pair[0]), ModelB: strings.TrimSpace(pair[1]), TraceDir: filepath.Join("reports", "traces")})
	if err != nil {
		log.Fatal(err)
	}
	pp, err := paypal.NewClient(cfg.APIBase, cfg.ClientID, cfg.ClientSecret, nil)
	if err != nil {
		log.Fatal(err)
	}
	seller, err := profit.LoadSeller(*sellerPath)
	if err != nil {
		log.Fatal(err)
	}
	store, err := flow.NewStore(filepath.Join(".data", "deals"))
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		deps: flow.Deps{
			Reader: rd, Writer: gem, WriterModel: *helperModel, Planner: gem, PlannerModel: *helperModel,
			Tools:   act.PayPalTools{Client: pp, Secrets: []string{cfg.SellerEmail, cfg.BuyerEmail}},
			Seller:  seller,
			Parties: act.Parties{SellerEmail: cfg.SellerEmail, BuyerEmail: cfg.BuyerEmail, ItemName: *item},
			Store:   store,
		},
		models: *models, helper: *helperModel, sellerPath: *sellerPath,
	}
	if err := s.init(); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stdout, "margin-check screen on http://%s (sandbox, drafts only)\n", *addr)
	log.Fatal(newHTTPServer(*addr, s.routes()).ListenAndServe())
}

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h}
}

type server struct {
	deps       flow.Deps
	tmpl       *template.Template
	csrf       string
	models     string
	helper     string
	sellerPath string
}

func (s *server) init() error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	s.csrf = hex.EncodeToString(b)
	t, err := template.New("page.html").Funcs(funcs()).ParseFS(pageFS, "page.html")
	s.tmpl = t
	return err
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("POST /check", s.post(s.check))
	mux.HandleFunc("GET /deal/{id}", s.show)
	mux.HandleFunc("POST /deal/{id}/reply", s.post(s.reply))
	mux.HandleFunc("POST /deal/{id}/approve", s.post(s.approve))
	return mux
}

// post wraps every state-changing request: it must carry this run's form token, so another web page in the
// same browser cannot press Approve on the seller's behalf.
func (s *server) post(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil || r.PostFormValue("csrf") != s.csrf {
			http.Error(w, "this form has expired; reload the page", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

type view struct {
	CSRF   string
	Deal   *flow.Deal
	Error  string
	Models string
	Helper string
	Seller string
	Sample string
}

func (s *server) render(w http.ResponseWriter, v view) {
	v.CSRF, v.Models, v.Helper, v.Seller = s.csrf, s.models, s.helper, s.sellerPath
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	if err := s.tmpl.Execute(w, v); err != nil {
		log.Println("render:", err)
	}
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	s.render(w, view{Sample: sampleThread})
}

func (s *server) check(w http.ResponseWriter, r *http.Request) {
	thread := strings.TrimSpace(r.PostFormValue("thread"))
	if thread == "" {
		s.render(w, view{Error: "Paste an email thread first.", Sample: sampleThread})
		return
	}
	deal, err := s.deps.Start(r.Context(), thread)
	if err != nil {
		s.render(w, view{Error: "The check stopped: " + err.Error(), Sample: thread})
		return
	}
	http.Redirect(w, r, "/deal/"+deal.ID, http.StatusSeeOther)
}

func (s *server) show(w http.ResponseWriter, r *http.Request) {
	deal, err := s.deps.Store.Load(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, view{Deal: deal})
}

func (s *server) reply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	text := strings.TrimSpace(r.PostFormValue("reply"))
	deal, err := s.deps.BuyerReplied(r.Context(), id, text)
	if err != nil {
		d, _ := s.deps.Store.Load(id)
		s.render(w, view{Deal: d, Error: "Reading the buyer's answer stopped: " + err.Error()})
		return
	}
	http.Redirect(w, r, "/deal/"+deal.ID+"#answer", http.StatusSeeOther)
}

func (s *server) approve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	deal, err := s.deps.ApproveAndRun(r.Context(), id, r.PostFormValue("fingerprint"))
	if err != nil && deal == nil {
		http.NotFound(w, r)
		return
	}
	if err != nil && deal.Result == nil {
		s.render(w, view{Deal: deal, Error: "Nothing was run: " + err.Error()})
		return
	}
	http.Redirect(w, r, "/deal/"+id+"#result", http.StatusSeeOther)
}

func funcs() template.FuncMap {
	return template.FuncMap{
		"money":       func(c int64, cur string) string { return offer.FormatMoney(c, cur) },
		"pct":         offer.FormatPercent,
		"label":       func(k offer.Kind) string { return labelOf(k) },
		"mask":        maskEmail,
		"maskID":      paypal.MaskID,
		"lines":       highlightLines,
		"add":         func(a, b int64) int64 { return a + b },
		"inc":         func(i int) int { return i + 1 },
		"fields":      fieldRows,
		"secs":        func(f float64) string { return fmt.Sprintf("%.1f s", f) },
		"fingerprint": act.Fingerprint,
	}
}

func labelOf(k offer.Kind) string {
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

func maskEmail(e string) string {
	at := strings.Index(e, "@")
	if at < 1 {
		return "***"
	}
	return e[:1] + "***" + e[at:]
}

const sampleThread = `Buyer:
Hi, we want 20 boxes of stoneware mugs at $40 per box for our shop.
We need free shipping and two months to pay.

Seller:
Sure, happy to help! Free shipping and 60 days is fine for 20 boxes.`
