package paypal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SandboxAPIBase is PayPal's official sandbox REST API base URL.
// Official doc: https://developer.paypal.com/api/rest/requests/
// Quoted URLs: Sandbox https://api-m.sandbox.paypal.com (Live https://api-m.paypal.com is rejected).
const SandboxAPIBase = "https://api-m.sandbox.paypal.com"

const (
	// ExpirySevenDaysAfterIssueDate is the PayPal enum value for a 7-day early payment discount window.
	// Official doc: https://developer.paypal.com/api/invoicing/v2/update-conditional-rule
	ExpirySevenDaysAfterIssueDate = "SEVEN_DAYS_AFTER_ISSUE_DATE"

	// PaymentTermNet30 sets the invoice due date 30 days after the invoice date.
	// Official doc: https://developer.paypal.com/api/invoicing/v2/invoices-create
	PaymentTermNet30 = "NET_30"

	// PaymentTermDueOnDateSpecified sets the invoice due date to the explicit detail.payment_term.due_date.
	// Official docs: https://developer.paypal.com/invoicing/object and https://developer.paypal.com/api/invoicing/v2/invoices-create
	PaymentTermDueOnDateSpecified = "DUE_ON_DATE_SPECIFIED"

	// MaxInvoiceNumberLen is PayPal's maximum length for detail.invoice_number (25 characters).
	// Official doc: https://developer.paypal.com/invoicing/object
	MaxInvoiceNumberLen = 25

	// IssueDuplicateInvoiceNumber is the 422 UNPROCESSABLE_ENTITY issue returned by PayPal Sandbox
	// when detail.invoice_number is reused on POST /v2/invoicing/invoices.
	IssueDuplicateInvoiceNumber = "DUPLICATE_INVOICE_NUMBER"
)

var (
	invoiceIDInHrefRe = regexp.MustCompile(`(INV2-[A-Za-z0-9-]+)`)
	bearerTokenRe     = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/-]+=*`)
)

// APIError carries PayPal's HTTP status code and error name without exposing any secrets.
// Official doc: https://developer.paypal.com/api/rest/responses/
// Non-identity error fields: name, message, debug_id, details[].issue.
// Identity (OAuth) error fields: error, error_description.
type APIError struct {
	StatusCode int
	Name       string
	Issue      string
}

func (e *APIError) Error() string {
	if e.Issue != "" {
		return fmt.Sprintf("paypal api error: status=%d name=%s issue=%s", e.StatusCode, e.Name, e.Issue)
	}
	return fmt.Sprintf("paypal api error: status=%d name=%s", e.StatusCode, e.Name)
}

// Client is a minimal, sandbox-only PayPal Invoicing v2 REST client.
// It caches the OAuth 2.0 bearer token in memory only and never sends an invoice.
type Client struct {
	baseURL      string
	clientID     string
	clientSecret string
	httpClient   *http.Client
	now          func() time.Time

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

// NewClient constructs a sandbox-only PayPal client.
// Official doc: https://developer.paypal.com/api/rest/requests/
// Only https://api-m.sandbox.paypal.com (or a local loopback http://127.0.0.1 / localhost test server) is accepted.
func NewClient(baseURL, clientID, clientSecret string, httpClient *http.Client) (*Client, error) {
	trimmedBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmedBase == "" {
		trimmedBase = SandboxAPIBase
	}
	if !isAllowedSandboxOrLoopbackURL(trimmedBase) {
		return nil, errors.New("paypal: base URL must be https://api-m.sandbox.paypal.com (sandbox only)")
	}
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil, errors.New("paypal: client credentials are required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		baseURL:      trimmedBase,
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   httpClient,
		now:          time.Now,
	}, nil
}

func isAllowedSandboxOrLoopbackURL(raw string) bool {
	if raw == SandboxAPIBase {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// FormatCents converts whole integer cents into a two-decimal currency value string (e.g. 1500 -> "15.00").
// Floats are never used.
func FormatCents(cents int64) (string, error) {
	if cents < 0 {
		return "", errors.New("paypal: money amount in cents cannot be negative")
	}
	dollars := cents / 100
	rem := cents % 100
	return fmt.Sprintf("%d.%02d", dollars, rem), nil
}

// MaskID returns only the last 4 characters of any identifier prefixed with "...".
func MaskID(id string) string {
	s := strings.TrimSpace(id)
	if s == "" {
		return "(none)"
	}
	if len(s) <= 4 {
		return "..." + s
	}
	return "..." + s[len(s)-4:]
}

// NewRequestID generates a unique caller-owned PayPal-Request-Id string.
func NewRequestID(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("paypal: failed to generate random request id")
	}
	p := strings.TrimSpace(prefix)
	if p == "" {
		return hex.EncodeToString(b[:]), nil
	}
	return p + "-" + hex.EncodeToString(b[:]), nil
}

// DeriveInvoiceNumber deterministically derives a unique detail.invoice_number (<= 25 characters)
// from a caller-owned idempotency key using a one-way SHA-256 digest so raw keys or secrets never
// appear in the customer-visible invoice_number field.
// Official docs:
//   - https://developer.paypal.com/invoicing/core-concepts (detail.invoice_number must be unique)
//   - https://developer.paypal.com/invoicing/object (invoice_number max 25 characters)
func DeriveInvoiceNumber(idempotencyKey string) (string, error) {
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return "", errors.New("paypal: idempotency key is required to derive detail.invoice_number")
	}
	sum := sha256.Sum256([]byte("margincheck-inv-v1:" + key))
	invNum := "MC-" + strings.ToUpper(hex.EncodeToString(sum[:])[:22])
	if len(invNum) > MaxInvoiceNumberLen {
		invNum = invNum[:MaxInvoiceNumberLen]
	}
	return invNum, nil
}

// EnsureToken fetches (or reuses an unexpired in-memory) OAuth 2.0 access token without returning the raw secret.
// Official doc: https://developer.paypal.com/api/rest/authentication/
// Endpoint: POST /v1/oauth2/token
// Request headers: Authorization (Basic client_id:client_secret), Content-Type: application/x-www-form-urlencoded, PayPal-Request-Id.
// Request form field: grant_type=client_credentials.
// Response JSON fields: access_token, token_type, app_id, expires_in.
func (c *Client) EnsureToken(ctx context.Context, requestID string) error {
	_, err := c.AccessToken(ctx, requestID)
	return err
}

// AccessToken returns an unexpired OAuth 2.0 access token cached in memory, requesting one if needed.
// Callers must never print or log the returned string; it exists only to attach Authorization headers or
// pass PAYPAL_ACCESS_TOKEN to the local PayPal MCP subprocess environment.
// Official doc: https://developer.paypal.com/api/rest/authentication/
// Endpoint: POST /v1/oauth2/token (grant_type=client_credentials; response: access_token, token_type, app_id, expires_in).
func (c *Client) AccessToken(ctx context.Context, requestID string) (string, error) {
	reqID := strings.TrimSpace(requestID)
	if reqID == "" {
		return "", errors.New("paypal: PayPal-Request-Id is required for POST /v1/oauth2/token")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && c.now().Before(c.tokenExpiry) {
		return c.accessToken, nil
	}
	return c.fetchTokenLocked(ctx, reqID)
}

func (c *Client) bearerToken(ctx context.Context, fallbackSeed string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && c.now().Before(c.tokenExpiry) {
		return c.accessToken, nil
	}
	reqID := strings.TrimSpace(fallbackSeed)
	if reqID != "" {
		reqID = "tok-" + reqID
	} else {
		var err error
		reqID, err = NewRequestID("tok")
		if err != nil {
			return "", err
		}
	}
	return c.fetchTokenLocked(ctx, reqID)
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	AppID       string `json:"app_id"`
	ExpiresIn   int64  `json:"expires_in"`
}

func (c *Client) fetchTokenLocked(ctx context.Context, requestID string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New(c.redactLocked(err.Error()))
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PayPal-Request-Id", requestID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", errors.New(c.redactLocked(err.Error()))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", errors.New(c.redactLocked(err.Error()))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", c.parseAPIErrorLocked(resp.StatusCode, body)
	}

	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", errors.New("paypal: invalid token response JSON")
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return "", errors.New("paypal: token response missing access_token")
	}

	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl > 2*time.Minute {
		ttl -= time.Minute
	} else if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	c.accessToken = tok.AccessToken
	c.tokenExpiry = c.now().Add(ttl)
	return c.accessToken, nil
}

// InvoiceItemInput describes one line item in whole cents for a draft invoice.
type InvoiceItemInput struct {
	Name            string
	Quantity        int64
	UnitPriceCents  int64
	DiscountPercent int64
}

// DraftInvoiceInput holds the fields needed to create a PayPal Invoicing v2 draft invoice.
type DraftInvoiceInput struct {
	CurrencyCode    string
	SellerEmail     string
	BuyerEmail      string
	InvoiceDate     string
	PaymentTermType string
	DueDate         string
	Items           []InvoiceItemInput
}

// Money represents PayPal's currency_code + value string structure.
type Money struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

// ItemDiscount represents items[].discount on a PayPal Invoicing v2 line item.
// Official docs: https://developer.paypal.com/invoicing/object and https://developer.paypal.com/api/invoicing/v2/invoices-create
type ItemDiscount struct {
	Percent string `json:"percent,omitempty"`
	Amount  *Money `json:"amount,omitempty"`
}

// InvoiceLineItem represents an entry in items[] on a PayPal Invoicing v2 invoice object.
type InvoiceLineItem struct {
	Name       string        `json:"name"`
	Quantity   string        `json:"quantity"`
	UnitAmount Money         `json:"unit_amount"`
	Discount   *ItemDiscount `json:"discount,omitempty"`
}

// EarlyPaymentDiscountSummary represents detail.payment_term.conditional_rules.early_payment_discount on an invoice.
type EarlyPaymentDiscountSummary struct {
	DiscountEndDate string `json:"discount_end_date,omitempty"`
	Percent         string `json:"percent,omitempty"`
	Amount          *Money `json:"amount,omitempty"`
	IsApplied       bool   `json:"is_applied,omitempty"`
}

// PaymentTermConditionalRules represents detail.payment_term.conditional_rules on an invoice.
type PaymentTermConditionalRules struct {
	EarlyPaymentDiscount *EarlyPaymentDiscountSummary `json:"early_payment_discount,omitempty"`
}

// InvoicePaymentTerm represents detail.payment_term on an invoice.
type InvoicePaymentTerm struct {
	TermType         string                       `json:"term_type,omitempty"`
	DueDate          string                       `json:"due_date,omitempty"`
	ConditionalRules *PaymentTermConditionalRules `json:"conditional_rules,omitempty"`
}

// InvoiceDetail represents detail on a PayPal Invoicing v2 invoice object.
type InvoiceDetail struct {
	CurrencyCode  string              `json:"currency_code"`
	InvoiceNumber string              `json:"invoice_number,omitempty"`
	InvoiceDate   string              `json:"invoice_date,omitempty"`
	PaymentTerm   *InvoicePaymentTerm `json:"payment_term,omitempty"`
}

// InvoiceConfiguration represents configuration on a PayPal Invoicing v2 invoice object.
type InvoiceConfiguration struct {
	HasConditionalRule bool `json:"has_conditional_rule,omitempty"`
}

// Link represents a HATEOAS link entry returned by PayPal REST APIs.
type Link struct {
	Href   string `json:"href"`
	Rel    string `json:"rel"`
	Method string `json:"method"`
}

// Invoice represents a PayPal Invoicing v2 invoice resource (always kept in DRAFT status).
type Invoice struct {
	ID             string               `json:"id"`
	Status         string               `json:"status"`
	Detail         InvoiceDetail        `json:"detail"`
	Items          []InvoiceLineItem    `json:"items,omitempty"`
	Configuration  InvoiceConfiguration `json:"configuration"`
	Href           string               `json:"href,omitempty"`
	Links          []Link               `json:"links,omitempty"`
	AlreadyExisted bool                 `json:"-"`
}

// HasEarlyPaymentDiscount reports whether GET /v2/invoicing/invoices/{invoice_id} shows a conditional rule attached.
func (inv *Invoice) HasEarlyPaymentDiscount() bool {
	if inv == nil {
		return false
	}
	if inv.Configuration.HasConditionalRule {
		return true
	}
	return inv.Detail.PaymentTerm != nil &&
		inv.Detail.PaymentTerm.ConditionalRules != nil &&
		inv.Detail.PaymentTerm.ConditionalRules.EarlyPaymentDiscount != nil
}

type createInvoicePayload struct {
	Detail            InvoiceDetail            `json:"detail"`
	Invoicer          *invoicerInfoPayload     `json:"invoicer,omitempty"`
	PrimaryRecipients []recipientInfoPayload   `json:"primary_recipients,omitempty"`
	Items             []invoiceLineItemPayload `json:"items"`
}

type invoicerInfoPayload struct {
	EmailAddress string `json:"email_address"`
}

type recipientInfoPayload struct {
	BillingInfo billingInfoPayload `json:"billing_info"`
}

type billingInfoPayload struct {
	EmailAddress string `json:"email_address"`
}

type invoiceLineItemPayload struct {
	Name       string        `json:"name"`
	Quantity   string        `json:"quantity"`
	UnitAmount Money         `json:"unit_amount"`
	Discount   *ItemDiscount `json:"discount,omitempty"`
}

// CreateDraftInvoice creates a draft invoice in PayPal Invoicing v2 and never sends it.
// It takes a caller-owned idempotencyKey, sets detail.invoice_number to DeriveInvoiceNumber(idempotencyKey)
// (within PayPal's 25-character limit), and sends PayPal-Request-Id: <idempotencyKey>.
// When PayPal answers 422 UNPROCESSABLE_ENTITY with issue DUPLICATE_INVOICE_NUMBER, CreateDraftInvoice
// finds the existing invoice by detail.invoice_number via SearchInvoiceByNumber and returns it with
// AlreadyExisted=true without creating a second invoice.
// Official docs:
//   - https://developer.paypal.com/api/invoicing/v2/invoices-create
//   - https://developer.paypal.com/invoicing/core-concepts
//   - https://developer.paypal.com/invoicing/object
//   - https://developer.paypal.com/api/invoicing/v2/invoices-search-invoices
//   - https://developer.paypal.com/api/invoicing/v2/schema.json
//
// Endpoint: POST /v2/invoicing/invoices
// Request headers: Authorization: Bearer <token>, Content-Type: application/json, Prefer: return=representation, PayPal-Request-Id.
// Request JSON fields: detail.currency_code, detail.invoice_number, detail.invoice_date, detail.payment_term.term_type,
// detail.payment_term.due_date, invoicer.email_address, primary_recipients[].billing_info.email_address,
// items[].name, items[].quantity, items[].unit_amount.currency_code, items[].unit_amount.value, items[].discount.percent.
// Response JSON fields: id, status, detail.invoice_number, detail.invoice_date, detail.currency_code,
// detail.payment_term.due_date, items[].discount.percent, configuration.has_conditional_rule.
func (c *Client) CreateDraftInvoice(ctx context.Context, idempotencyKey string, in DraftInvoiceInput) (*Invoice, error) {
	reqID := strings.TrimSpace(idempotencyKey)
	if reqID == "" {
		return nil, errors.New("paypal: PayPal-Request-Id is required for POST /v2/invoicing/invoices")
	}
	invNum, err := DeriveInvoiceNumber(reqID)
	if err != nil {
		return nil, err
	}
	currency := strings.ToUpper(strings.TrimSpace(in.CurrencyCode))
	if len(currency) != 3 {
		return nil, errors.New("paypal: 3-letter currency_code is required")
	}
	seller := strings.TrimSpace(in.SellerEmail)
	buyer := strings.TrimSpace(in.BuyerEmail)
	if seller == "" || buyer == "" {
		return nil, errors.New("paypal: seller and buyer email addresses are required")
	}
	if len(in.Items) == 0 {
		return nil, errors.New("paypal: at least one invoice item is required")
	}

	items := make([]invoiceLineItemPayload, 0, len(in.Items))
	for _, it := range in.Items {
		name := strings.TrimSpace(it.Name)
		if name == "" {
			return nil, errors.New("paypal: item name is required")
		}
		if it.Quantity <= 0 {
			return nil, errors.New("paypal: item quantity must be positive")
		}
		if it.DiscountPercent < 0 || it.DiscountPercent > 100 {
			return nil, errors.New("paypal: item discount percent must be between 0 and 100")
		}
		val, err := FormatCents(it.UnitPriceCents)
		if err != nil {
			return nil, err
		}
		line := invoiceLineItemPayload{
			Name:     name,
			Quantity: strconv.FormatInt(it.Quantity, 10),
			UnitAmount: Money{
				CurrencyCode: currency,
				Value:        val,
			},
		}
		if it.DiscountPercent > 0 {
			line.Discount = &ItemDiscount{
				Percent: strconv.FormatInt(it.DiscountPercent, 10),
			}
		}
		items = append(items, line)
	}

	detail := InvoiceDetail{
		CurrencyCode:  currency,
		InvoiceNumber: invNum,
		InvoiceDate:   strings.TrimSpace(in.InvoiceDate),
	}
	term := strings.TrimSpace(in.PaymentTermType)
	dueDate := strings.TrimSpace(in.DueDate)
	if term != "" || dueDate != "" {
		detail.PaymentTerm = &InvoicePaymentTerm{
			TermType: term,
			DueDate:  dueDate,
		}
	}

	payload := createInvoicePayload{
		Detail:   detail,
		Invoicer: &invoicerInfoPayload{EmailAddress: seller},
		PrimaryRecipients: []recipientInfoPayload{
			{BillingInfo: billingInfoPayload{EmailAddress: buyer}},
		},
		Items: items,
	}

	rawReq, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("paypal: failed to encode draft invoice JSON")
	}

	token, err := c.bearerToken(ctx, reqID)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v2/invoicing/invoices", bytes.NewReader(rawReq))
	if err != nil {
		return nil, errors.New(c.redact(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Prefer", "return=representation")
	req.Header.Set("PayPal-Request-Id", reqID)

	resp, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		parsedErr := c.parseAPIError(resp.StatusCode, body)
		var apiErr *APIError
		if resp.StatusCode == http.StatusUnprocessableEntity && errors.As(parsedErr, &apiErr) && apiErr.Issue == IssueDuplicateInvoiceNumber {
			existing, searchErr := c.SearchInvoiceByNumber(ctx, "search-"+reqID, invNum)
			if searchErr != nil {
				return nil, searchErr
			}
			existing.AlreadyExisted = true
			if existing.Detail.InvoiceNumber == "" {
				existing.Detail.InvoiceNumber = invNum
			}
			return existing, nil
		}
		return nil, parsedErr
	}

	var inv Invoice
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &inv); err != nil {
			return nil, errors.New("paypal: invalid draft invoice response JSON")
		}
	}
	if inv.ID == "" {
		inv.ID = extractInvoiceID(&inv)
	}
	if inv.ID == "" {
		return nil, errors.New("paypal: draft invoice response missing id")
	}
	if inv.Detail.InvoiceNumber == "" {
		inv.Detail.InvoiceNumber = invNum
	}
	return &inv, nil
}

type searchInvoicesPayload struct {
	InvoiceNumber string `json:"invoice_number"`
}

// SearchInvoiceByNumber finds an invoice by its unique detail.invoice_number (<= 25 characters).
// Official doc: https://developer.paypal.com/api/invoicing/v2/invoices-search-invoices
// Endpoint: POST /v2/invoicing/search-invoices?page=1&page_size=20&total_required=true
// Request headers: Authorization: Bearer <token>, Content-Type: application/json, PayPal-Request-Id.
// Request JSON field: invoice_number (0..25 characters).
// Response JSON fields: total_items, total_pages, items[].id, items[].status, items[].detail.invoice_number.
func (c *Client) SearchInvoiceByNumber(ctx context.Context, requestID, invoiceNumber string) (*Invoice, error) {
	reqID := strings.TrimSpace(requestID)
	if reqID == "" {
		return nil, errors.New("paypal: PayPal-Request-Id is required for POST /v2/invoicing/search-invoices")
	}
	invNum := strings.TrimSpace(invoiceNumber)
	if invNum == "" || len(invNum) > MaxInvoiceNumberLen {
		return nil, errors.New("paypal: invoice_number (1..25 chars) is required for search")
	}

	token, err := c.bearerToken(ctx, reqID)
	if err != nil {
		return nil, err
	}

	rawReq, err := json.Marshal(searchInvoicesPayload{InvoiceNumber: invNum})
	if err != nil {
		return nil, errors.New("paypal: failed to encode search-invoices JSON")
	}

	endpoint := c.baseURL + "/v2/invoicing/search-invoices?page=1&page_size=20&total_required=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawReq))
	if err != nil {
		return nil, errors.New(c.redact(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PayPal-Request-Id", reqID)

	resp, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseAPIError(resp.StatusCode, body)
	}

	var list InvoiceList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, errors.New("paypal: invalid search-invoices response JSON")
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.ID == "" {
			item.ID = extractInvoiceID(item)
		}
		if item.Detail.InvoiceNumber == invNum && item.ID != "" {
			cp := *item
			return &cp, nil
		}
	}
	if len(list.Items) == 1 {
		item := list.Items[0]
		if item.ID == "" {
			item.ID = extractInvoiceID(&item)
		}
		if item.ID != "" {
			return &item, nil
		}
	}

	// Fallback to GET /v2/invoicing/invoices if search index lags immediately after creation.
	fallbackList, listErr := c.ListInvoices(ctx, 1, 100, true)
	if listErr == nil && fallbackList != nil {
		for i := range fallbackList.Items {
			item := &fallbackList.Items[i]
			if item.ID == "" {
				item.ID = extractInvoiceID(item)
			}
			if item.Detail.InvoiceNumber == invNum && item.ID != "" {
				cp := *item
				return &cp, nil
			}
		}
	}

	return nil, errors.New("paypal: duplicate invoice_number reported but existing invoice not found")
}

func extractInvoiceID(inv *Invoice) string {
	if m := invoiceIDInHrefRe.FindStringSubmatch(inv.Href); len(m) > 1 {
		return m[1]
	}
	for _, l := range inv.Links {
		if m := invoiceIDInHrefRe.FindStringSubmatch(l.Href); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// EarlyPaymentDiscountInput configures an EARLY_PAYMENT_DISCOUNT conditional rule for an invoice.
type EarlyPaymentDiscountInput struct {
	InvoiceID       string
	Percent         int64
	ExpiryCondition string
	EndDate         string
}

// RuleExpiryTerms represents rule_expiry_terms on a PayPal conditional rule.
type RuleExpiryTerms struct {
	RuleExpiryCondition  string `json:"rule_expiry_condition"`
	ConditionRuleEndDate string `json:"condition_rule_end_date,omitempty"`
}

// ConditionalRule represents one entry in rules[] on PayPal's conditional-rules endpoints.
type ConditionalRule struct {
	ConditionalRuleID        string          `json:"conditional_rule_id,omitempty"`
	ConditionalRuleType      string          `json:"conditional_rule_type"`
	ConditionalRuleValueType string          `json:"conditional_rule_value_type"`
	ConditionalRuleValue     string          `json:"conditional_rule_value"`
	RuleExpiryTerms          RuleExpiryTerms `json:"rule_expiry_terms"`
}

// ConditionalRulesResponse represents the JSON payload and response wrapper { "rules": [...] }.
type ConditionalRulesResponse struct {
	Rules []ConditionalRule `json:"rules"`
}

// CreateEarlyPaymentDiscountRule attaches an EARLY_PAYMENT_DISCOUNT conditional rule to a draft invoice.
// Official docs:
//   - https://developer.paypal.com/api/invoicing/v2/create-conditional-rules
//   - https://developer.paypal.com/api/invoicing/v2/update-conditional-rule
//
// Endpoint: POST /v2/invoicing/invoices/{id}/conditional-rules
// Request headers: Authorization: Bearer <token>, Content-Type: application/json, PayPal-Request-Id (length 1..10000).
// Request JSON fields: rules[].conditional_rule_type ("EARLY_PAYMENT_DISCOUNT"),
// rules[].conditional_rule_value_type ("PERCENT"), rules[].conditional_rule_value,
// rules[].rule_expiry_terms.rule_expiry_condition, rules[].rule_expiry_terms.condition_rule_end_date.
// Response JSON fields: rules[].conditional_rule_id, rules[].conditional_rule_type,
// rules[].conditional_rule_value_type, rules[].conditional_rule_value, rules[].rule_expiry_terms.
func (c *Client) CreateEarlyPaymentDiscountRule(ctx context.Context, requestID string, in EarlyPaymentDiscountInput) (*ConditionalRulesResponse, error) {
	reqID := strings.TrimSpace(requestID)
	if reqID == "" || len(reqID) > 10000 {
		return nil, errors.New("paypal: PayPal-Request-Id (1..10000 chars) is required for POST /v2/invoicing/invoices/{id}/conditional-rules")
	}
	invID := strings.TrimSpace(in.InvoiceID)
	if invID == "" {
		return nil, errors.New("paypal: invoice id is required for conditional rules")
	}
	if in.Percent <= 0 || in.Percent > 100 {
		return nil, errors.New("paypal: discount percent must be an integer between 1 and 100")
	}
	cond := strings.TrimSpace(in.ExpiryCondition)
	if cond == "" {
		cond = ExpirySevenDaysAfterIssueDate
	}

	payload := ConditionalRulesResponse{
		Rules: []ConditionalRule{
			{
				ConditionalRuleType:      "EARLY_PAYMENT_DISCOUNT",
				ConditionalRuleValueType: "PERCENT",
				ConditionalRuleValue:     strconv.FormatInt(in.Percent, 10),
				RuleExpiryTerms: RuleExpiryTerms{
					RuleExpiryCondition:  cond,
					ConditionRuleEndDate: strings.TrimSpace(in.EndDate),
				},
			},
		},
	}

	rawReq, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("paypal: failed to encode conditional rules JSON")
	}

	token, err := c.bearerToken(ctx, reqID)
	if err != nil {
		return nil, err
	}

	endpoint := c.baseURL + "/v2/invoicing/invoices/" + url.PathEscape(invID) + "/conditional-rules"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawReq))
	if err != nil {
		return nil, errors.New(c.redact(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PayPal-Request-Id", reqID)

	resp, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return nil, c.parseAPIError(resp.StatusCode, body)
	}

	var out ConditionalRulesResponse
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, errors.New("paypal: invalid conditional rules response JSON")
		}
	}
	return &out, nil
}

// GetInvoice retrieves full details for an invoice by ID.
// Official doc: https://developer.paypal.com/api/invoicing/v2/invoices-get
// Endpoint: GET /v2/invoicing/invoices/{invoice_id}
// Request header: Authorization: Bearer <token>.
// Response JSON fields: id, status, detail.currency_code, detail.invoice_date,
// detail.payment_term.conditional_rules.early_payment_discount, configuration.has_conditional_rule.
func (c *Client) GetInvoice(ctx context.Context, invoiceID string) (*Invoice, error) {
	invID := strings.TrimSpace(invoiceID)
	if invID == "" {
		return nil, errors.New("paypal: invoice_id is required")
	}
	token, err := c.bearerToken(ctx, "")
	if err != nil {
		return nil, err
	}

	endpoint := c.baseURL + "/v2/invoicing/invoices/" + url.PathEscape(invID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New(c.redact(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseAPIError(resp.StatusCode, body)
	}

	var inv Invoice
	if err := json.Unmarshal(body, &inv); err != nil {
		return nil, errors.New("paypal: invalid invoice response JSON")
	}
	return &inv, nil
}

// InvoiceList represents the response from GET /v2/invoicing/invoices.
type InvoiceList struct {
	TotalItems int       `json:"total_items"`
	TotalPages int       `json:"total_pages"`
	Items      []Invoice `json:"items"`
}

// ListInvoices lists invoices with optional pagination and total_required=true.
// Official doc: https://developer.paypal.com/api/invoicing/v2/invoices-list
// Endpoint: GET /v2/invoicing/invoices
// Query parameters: page, page_size (1..100, default 20), total_required (boolean).
// Response JSON fields: total_items, total_pages, items.
func (c *Client) ListInvoices(ctx context.Context, page, pageSize int, totalRequired bool) (*InvoiceList, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		return nil, errors.New("paypal: page_size must be between 1 and 100")
	}

	token, err := c.bearerToken(ctx, "")
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(pageSize))
	q.Set("total_required", strconv.FormatBool(totalRequired))

	endpoint := c.baseURL + "/v2/invoicing/invoices?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New(c.redact(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseAPIError(resp.StatusCode, body)
	}

	var list InvoiceList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, errors.New("paypal: invalid invoice list response JSON")
	}
	return &list, nil
}

func (c *Client) do(req *http.Request) (*http.Response, []byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, errors.New(c.redact(err.Error()))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, nil, errors.New(c.redact(err.Error()))
	}
	return resp, body, nil
}

type rawPayPalError struct {
	Name    string `json:"name"`
	Error   string `json:"error"`
	Details []struct {
		Issue string `json:"issue"`
	} `json:"details"`
}

func (c *Client) parseAPIError(statusCode int, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.parseAPIErrorLocked(statusCode, body)
}

func (c *Client) parseAPIErrorLocked(statusCode int, body []byte) error {
	var raw rawPayPalError
	_ = json.Unmarshal(body, &raw)

	name := strings.TrimSpace(raw.Name)
	if name == "" {
		name = strings.TrimSpace(raw.Error)
	}
	if name == "" {
		name = fmt.Sprintf("HTTP_%d", statusCode)
	}

	var issue string
	if len(raw.Details) > 0 {
		issue = strings.TrimSpace(raw.Details[0].Issue)
	}

	return &APIError{
		StatusCode: statusCode,
		Name:       c.redactLocked(name),
		Issue:      c.redactLocked(issue),
	}
}

func (c *Client) redact(s string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.redactLocked(s)
}

func (c *Client) redactLocked(s string) string {
	if s == "" {
		return ""
	}
	out := bearerTokenRe.ReplaceAllString(s, "[REDACTED]")
	basicRaw := c.clientID + ":" + c.clientSecret
	basicB64 := base64.StdEncoding.EncodeToString([]byte(basicRaw))

	for _, secret := range []string{c.accessToken, c.clientSecret, c.clientID, basicB64, basicRaw} {
		if secret != "" {
			out = strings.ReplaceAll(out, secret, "[REDACTED]")
		}
	}
	return out
}
