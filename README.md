# margin-check

A profit spell-checker for small sellers who negotiate custom or wholesale orders over email and bill through PayPal Invoicing.

The app reads pasted email threads, calculates real kept profit in whole cents across hidden giveaways (discounts, free shipping, payment terms, rebates, fees, and double discounting), suggests fair counter-offers, and prepares **draft** PayPal invoices with early-payment conditional rules in the PayPal Sandbox. It never sends emails or invoices automatically.

## Setup

1. Copy `.env.example` to `.env` (git-ignored) and fill in your PayPal Sandbox credentials (`https://api-m.sandbox.paypal.com`), US Business and US Personal sandbox account emails, and `GEMINI_API_KEY` (used from Step 2 onwards).
2. Run checks and tests:
   ```bash
   go vet ./...
   go test ./...
   ```
3. Run the PayPal sandbox proof:
   ```bash
   go run ./cmd/sandboxcheck
   ```
