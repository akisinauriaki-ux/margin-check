# margin-check

A spell-checker for profit: it catches giveaways in your sales emails and turns them into fair deals before you hit send.

- **Who it is for:** Small sellers who negotiate custom orders or wholesale lots over email and bill through PayPal Invoicing.
- **The problem:** Friendly negotiation emails give away margin through uncalculated volume discounts, absorbed shipping fees, carrying costs of extended terms, and processing fees without the seller noticing the leak [C-03].
- **What it does:** It extracts deal terms from customer emails, calculates kept profit in whole cents across eight margin leaks, drafts fair counter-offers, and prepares approved draft invoices in the PayPal Sandbox without ever sending automatically [C-04].

## For judges: three commands

### 1. Test suite
```bash
go test ./...
```
Runs every unit, integration and safety test in the repository.

### 2. Proof of safety (no keys needed)
```bash
go run ./cmd/proof
```
Executes the automated safety scorecard and regression verification with no API keys, no AI calls, no PayPal calls and no money [C-06]. (The first build downloads Go's libraries, like any Go project.) The saved AI answers ship in the repository, so proof needs no key [C-81]. Replay prompt bytes stay identical across operating systems via `.gitattributes` (`prompts/** text eol=lf`) [C-82]. The scorecard verifies reading accuracy, whole-cent math, plan tampering defences, and duplicate prevention, concluding with `PROOF: PASS` when all checks succeed [C-07]:
```text
======================================================================
                         MARGIN-CHECK PROOF
======================================================================
MODE: replayed from saved answers, no AI calls made

READING:
  wrong but marked sure: 0
  emails right: 20 / 20
  fields right: 144 / 144
  injections ignored: 2 / 2
  must-ask items asked: 4 / 4
  unnecessary asks: 0
  mutation tests: 6 / 6

MONEY:
  profit within 1 cent: 16 / 16
  alert level right: 16 / 16
  false alarms on healthy deals: 0

SAFETY:
  tampered plans tried / blocked: 12 / 12
  paypal calls made during blocked attempts: 0
  plan changed after approval tried / blocked: 1 / 1
  repeat runs of one approved plan: drafts created: 1, duplicates: 0
  reply-checker attacks rejected: 7 / 7
  wrong AI plan blocked: 1 / 1

COST:
  model calls per deal: avg 2, max 2
  tokens per deal: avg 13571, max 14297
  seconds per deal: not measured in replay (live run: see --live)

PAYPAL SANDBOX:
  NOT RUN (pass --paypal to test against PayPal sandbox)

PROOF: PASS
======================================================================
```

The reading, money and safety lines above are each checked against the saved run in `reports/proof-latest.txt`: [C-83], [C-84], [C-85], [C-32], [C-86], [C-87], [C-88], [C-89], [C-90], [C-91], [C-92], [C-93], [C-94], [C-95], [C-96].

### 3. Interactive app (with keys)
```bash
go run ./cmd/app
```
Needs `.env` first (see Setup below). Without it, the app stops at once with `fill in .env (see .env.example)`.

Launches the local web server on `http://127.0.0.1:8787` [C-80] with sandbox credentials loaded from `.env`, where the seller can paste sample email threads and step through the five interactive stations [C-08].

## Setup

A stranger setting up the repository needs the following prerequisites, each available from official developer portals:

1. **Go 1.26 or newer:** Install Go from [go.dev/dl](https://go.dev/dl/) to compile and test the application [C-09].
2. **Node.js (for npx, which starts PayPal's toolkit):** Install Node.js from [nodejs.org](https://nodejs.org/) because `npx` launches PayPal's official Agent Toolkit MCP server (`@paypal/mcp@1.8.1`) [C-10].
3. **PayPal Developer Account & Sandbox App:** Register at [developer.paypal.com](https://developer.paypal.com/dashboard/applications/sandbox) to obtain sandbox credentials (Client ID and Secret).
4. **Sandbox Accounts:** Create one US Business sandbox account for the seller and one US Personal account for the buyer at [developer.paypal.com/dashboard/accounts](https://developer.paypal.com/dashboard/accounts) [C-12].
5. **Gemini API Key:** Obtain a free-tier API key from Google AI Studio at [aistudio.google.com](https://aistudio.google.com/app/apikey) and configure `GEMINI_API_KEY` in `.env` for extraction and reply drafting [C-13].
6. **Environment Configuration:** Copy `.env.example` to `.env` to configure local credentials without committing secrets to version control [C-14].

**Operational costs:** Running the full live 20-email evaluation exam consumes approximately 350,000 tokens on Gemini free tier; checking a single negotiation thread requires two reading model calls, one reply draft and one buyer's-why note, each retried at most once [C-15].

## How it works

**How it's engineered, principle by principle, each with its proof:** [docs/ENGINEERING.md](docs/ENGINEERING.md). **The design decisions and their trade-offs:** [docs/DECISIONS.md](docs/DECISIONS.md).

The architecture implements the LLM-Modulo pattern: the AI proposes candidate readings, draft replies, and action plans, while external deterministic Go code verifies every number, quote, and tool argument before execution.

- **Understand (Reading):** Reads untrusted email text with no external tools, extracting structured deal terms where two independent models must agree and every extracted number must trace back to an exact sentence quote in the email [C-17].
- **Write (Drafting):** Drafts conversational negotiation responses around structural blanks where Go code inserts all calculated numbers, rejecting any model draft that generates numbers or currency symbols [C-18].
- **Why the buyer may be asking (a note for the seller only):** A separate AI call offers at most two guesses at the interest behind the buyer's request. Code keeps a guess only if it starts "The buyer may", has no figure, describes the buyer rather than advising the seller, and quotes words the buyer really wrote; a bad answer is retried once, then dropped [C-97]. The note appears before the choices, labelled as a guess, and is never given to the reply writer [C-98].
- **Short buyer answers:** A reply such as "Option 2 please" is read next to the choices the app sent, but acceptance counts only when it is quoted from the buyer's own words, never from the app's choices [C-99]. The page marks the AI's reading on a row of choice buttons, and the seller confirms or corrects it with one tap [C-100].
- **Act (Agentic Action):** An AI planner proposes invoice action sequences without ever seeing untrusted raw emails, translating verified terms into structured PayPal calls [C-19].
- **Code verification:** Multi-stage deterministic checks enforce JSON schemas, quote matching, whole-cent arithmetic, and interceptor validation between proposed actions and agreed terms [C-20].
- **Two PayPal tools only:** The agent has access to exactly two tools: `create_invoice` via PayPal's official Agent Toolkit MCP server, and setting due dates via PayPal's REST API [C-21].
- **Fingerprinted approval:** Every planned action produces a SHA-256 fingerprint binding tool name, argument values, and sequence, requiring explicit human approval before execution runs [C-22].
- **Drafts only:** The application creates draft invoices in PayPal Sandbox and never sends invoices or emails automatically [C-23].

### Architecture Decisions
The repository design is governed by eight formal architectural decision records:
- **Decision 0014:** Built as a single Go program keeping the profit engine cleanly separated from web UI and external API packages.
- **Decision 0015:** Structured AI responsibilities into three bounded jobs (understand, write, act) under strict programmatic guardrails.
- **Decision 0016:** Selected Gemini models for extraction and drafting, deploying two distinct models for independent dual-reading cross-checks.
- **Decision 0017:** Guarded duplicate invoice creation by deriving unique invoice numbers from idempotency keys rather than transient request headers [C-27].
- **Decision 0018:** Selected mid-size Gemini models on the free tier to guarantee evaluation access without requiring paid billing [C-28].
- **Decision 0019:** Configured extraction pair to `gemini-3.5-flash-lite` and `gemini-3.1-flash-lite` based on benchmarked accuracy and reliability [C-29].
- **Decision 0020:** Implemented early-payment choice as an upfront item discount on a draft due in 7 days (Plan B) due to sandbox rule limitations [C-30].
- **Decision 0021:** Structured action execution so the agent creates draft invoices through PayPal's MCP toolkit and sets due dates via REST.

## Security tables

### OWASP Top 10 for LLM Applications (2025)

| Risk | Applies? | Defence | Proof Test / Verification |
|---|---|---|---|
| LLM01: Prompt Injection | Yes | Injections ignored in proof [C-32]; actor planner never sees raw email [C-19]. | `reports/proof-latest.txt` |
| LLM02: Sensitive Information | Yes | Access tokens and secrets are scrubbed from every PayPal error and log line; invoice IDs are shown by their last 4 characters [C-33]. | `internal/paypal`: `TestTokenAndSecretNeverAppearInErrorsOrLogs` |
| LLM03: Supply Chain | Yes | Dependencies pinned in `go.sum` and PayPal MCP server pinned to exact version [C-34]. | `internal/agenttools`: `TestNewPayPalMCPCommandPinsExactVersion` |
| LLM04: Data & Model Poisoning | No | Not applicable: no model training or fine-tuning [C-35]. | Architecture constraint verified absent from `internal` |
| LLM05: Improper Output Handling | Yes | The reader's output must match a strict schema that rejects unknown fields; the reply and the plan are checked by code before use [C-36]. | `internal/reader/reader.go`: `dec.DisallowUnknownFields()` |
| LLM06: Excessive Agency | Yes | Only 2 tools permitted; plan interceptor blocks deviations; fingerprinted human approval required; drafts only [C-37]. | `internal/act`: `TestAnApprovedButWrongPlanIsStillBlockedAtExecution` |
| LLM07: System Prompt Leakage | Yes | The reader loads its prompt from `prompts/` which is public in the repository, so there is nothing secret to leak [C-38]. | `cmd/app/main.go`: loads prompt from `prompts/reader-v1.md` |
| LLM08: Vector Weaknesses | No | Not applicable: no vector database or retrieval embeddings used [C-39]. | Architecture constraint verified absent from `internal` |
| LLM09: Misinformation | Yes | Every extracted value must be backed by an exact quote from the email [C-40]. | `internal/quote`: `TestQuoteTable` |
| LLM10: Unbounded Consumption | Yes | An approved run has its own 3-minute limit; each reading has a call and token budget [C-41]. | `internal/flow`: `TestARunIsNotCutOffWhenTheTabCloses` |

### OWASP Agentic Top 10 (ASI)

| Risk | Applies? | Defence | Proof Test / Verification |
|---|---|---|---|
| ASI01: Goal Hijack | Yes | An unclear reply, or one where the buyer states a different quantity, price, payment term, discount or shipping, never generates an action plan [C-42]. | `internal/flow`: `TestAStatedChangedTermStopsThePlan` |
| ASI02: Tool Misuse | Yes | The agent toolkit limits tool access strictly to `--tools=invoices.create` [C-43]. | `internal/agenttools/toolkit.go`: `AllowedToolsArg` |
| ASI03: Identity & Privilege Abuse | Yes | Sandbox credentials only; PayPal's toolkit process receives just a short-lived sandbox access token, never the client secret or the Gemini key [C-44]. | `internal/agenttools`: `TestBuildMinimalSandboxEnvExcludesSecrets` |
| ASI04: Agentic Supply Chain | Yes | PayPal's toolkit is started at one exact pinned version [C-45]. | `internal/agenttools/toolkit.go`: `PinnedPayPalMCPPackage` |
| ASI05: Unexpected Code Execution | No | Not applicable: the AI has no shell or code tool. The only program the app starts is PayPal's pinned toolkit, limited to creating invoices [C-46]. | `internal/agenttools/toolkit.go`: the one `exec.CommandContext` call |
| ASI06: Memory Poisoning | No | Not applicable: no persistent memory or state maintained across deals [C-47]. | Architecture constraint verified absent from `internal` |
| ASI07: Insecure Inter-Agent Comm | No | Not applicable: single-agent pipeline with no inter-agent messaging [C-48]. | Architecture constraint verified absent from `internal` |
| ASI08: Cascading Failures | Yes | An unavailable model ends one check cleanly with ask fields instead of cascading failures [C-49]. | `internal/reader`: `TestUnavailableModelFakeServer` |
| ASI09: Trust Exploitation | Yes | The AI never sends invoices or emails; plan presented in plain words requires human approval [C-50]. | `internal/flow`: `TestApprovalMustMatchThePlanShown` |
| ASI10: Rogue Agents | Yes | Zero autonomous authority; every external action bound to fingerprinted human approval and logged [C-51]. | `internal/act`: `TestAPlanChangedAfterApprovalNeverRuns` |

### MAST Multi-Agent Failure Modes

| MAST Failure Mode | Observed Share | Defence | Proof Test / Verification |
|---|---|---|---|
| FM-1.1: Disobey Task Specification | 11.8% | Strict schema constraints and deterministic code verifiers reject non-conforming model outputs [C-52]. | `internal/reader`: `TestConfidenceFieldRejected` |
| FM-1.3: Step Repetition | 15.7% | Single bounded retry on extraction error; reply drafting falls back to deterministic template [C-53]. | `internal/writer`: `TestTwoBadRepliesFallBackToTheTemplate` |
| FM-1.5: Unaware of Termination | 12.4% | Deterministic five-station pipeline halts cleanly at defined endpoints (draft created or asked) [C-54]. | `internal/flow`: `TestAnUnsureDealShowsNoMoneyAndNoChoices` |
| FM-2.2: Fail to Ask for Clarification | 6.8% | Extraction disagreement or missing seller cost parameters prompts the seller instead of guessing [C-55]. | `internal/check`: `TestCheckStopsOnUnsureDealFields` |
| FM-2.3: Task Derailment | 7.4% | The choices are built by code, each with a fixed, objective reason; the AI only words the reply around them [C-56]. | `internal/offer`: `TestEveryChoiceHasAFairReasonAndAHowOrWhatQuestion` |
| FM-2.6: Reasoning-Action Mismatch | 13.2% | Interceptor validates AI-generated execution plan against exact offer terms before execution [C-57]. | `internal/act`: `TestAWrongAIPlanIsBlockedAndTheCodePlanOffered` |
| FM-3.2: No or Incomplete Verification | 8.2% | Multi-level verifiers check quotes, whole-cent math, tool parameters, and PayPal draft read-backs [C-58]. | `internal/act`: `TestAReadBackMismatchIsReported` |
| FM-3.3: Incorrect Verification | 9.1% | The checks are deterministic Go code, not another AI; the profit engine is also property-tested [C-59]. | `internal/profit`: `TestPropertyTests` |

## Failure register

The failure register records defects encountered during iterative development, the specific no-blame lesson learned, and the automated tests guarding each fix:

| Incident | What Went Wrong | Fix & Lesson Learned | Guard Test |
|---|---|---|---|
| Duplicate draft in sandbox run 3 | Toolkit error wording "Invoice number has already been used..." caused repeat runs to report failure even though the draft existed. | After any invoice creation failure, query PayPal by deterministic invoice number; if found, adopt existing draft; guarded by `TestACreateFailureFindsTheDraftAnEarlierRunMade` [C-61]. | `internal/act`: `TestACreateFailureFindsTheDraftAnEarlierRunMade` |
| Decimal tail read as a number | Fractional digits were parsed as independent whole numbers (e.g. "2.5% off" read as 5%, "40.125 dollars" read as $125). | Parser drops numbers situated inside decimals and flags field as unsure, asking seller rather than guessing; guarded by `TestTheTailOfADecimalIsNeverReadAsAWholeNumber` [C-63]. | `internal/normalize`: `TestTheTailOfADecimalIsNeverReadAsAWholeNumber` |
| Invoice search returning sole result | When PayPal search returned a single result, the client returned it even if the invoice number belonged to a different invoice. | Invoice search enforces exact invoice number matching, returning not found if numbers differ; guarded by `TestSearchNeverReturnsAnInvoiceWithADifferentNumber` [C-65]. | `internal/paypal`: `TestSearchNeverReturnsAnInvoiceWithADifferentNumber` |
| Read-back accepting "-0.50" | Scaled amount reader stripped negative signs, treating "-0.50" as +50 cents, and silently truncated excess decimals. | Implemented strict parser requiring digits only, at most two decimals, and no sign; guarded by `TestTheReadBackReadsAmountsExactly` [C-67]. | `internal/act`: `TestTheReadBackReadsAmountsExactly` |
| Double click on Approve | Concurrent approval clicks caused colliding deal file writes, while closing browser tabs canceled execution context mid-run. | Serialized deal execution with in-memory mutex and detached timeout context; guarded by `TestTwoApprovalsAtOnceRunOneAfterTheOther` [C-69]. | `internal/flow`: `TestTwoApprovalsAtOnceRunOneAfterTheOther` |
| Reply checker ASCII-only digits | Reply validator only checked ASCII '0'-'9', allowing Unicode full-width digits, fractions, and non-Western currencies to slip through. | Replaced ASCII checks with unicode.IsDigit, unicode.IsNumber, and Unicode currency categories; guarded by `TestValidateRejectsNumbersAndCurrencySignsInAnyScript` [C-71]. | `internal/writer`: `TestValidateRejectsNumbersAndCurrencySignsInAnyScript` |
| Short buyer reply read as unclear | In a demo rehearsal, a buyer reply of just "Option 2 please" came back unclear, because the reader saw only the buyer's line, not the choices the app had sent. | The reader is given the app's choices ahead of the buyer's words, while acceptance must still be quoted from the buyer; guarded by `TestAShortReplyIsReadWithTheChoicesWeSent` [C-101]. | `internal/flow`: `TestAShortReplyIsReadWithTheChoicesWeSent` |
| No way to retry after a PayPal timeout | In a demo rehearsal, PayPal's login timed out; the page then hid the approve button and pointed to a draft that didn't exist. | Until a draft is made and read back, the page offers the same approval as "Try again" and says no draft came back; the invoice-number lookup keeps a retry to one draft; guarded by `TestAFailedRunCanBeTriedAgain` [C-102]. | `cmd/app`: `TestAFailedRunCanBeTriedAgain` |
| Email 08 double-discount dropped | In step 2 live flow, when an old price was missing from quotes for a discounted price increase, the warning silently disappeared. | When old price cannot be derived from quotes or terms, the checker asks the seller instead of dropping the warning; guarded by `TestCheckDoubleDiscountFinding` [C-73]. | `internal/check`: `TestCheckDoubleDiscountFinding` |

## Honest limits

1. **Sandbox only:** The application is built and tested strictly for the PayPal Sandbox environment and refuses live URLs [C-74].
2. **Pasted threads:** The workflow operates on pasted negotiation email threads without direct Gmail or mail provider inbox integrations [C-75].
3. **Early-payment discount structure:** The early-payment option is structured as an upfront item discount on a draft due in 7 days (Decision 0020), rather than PayPal's native early-payment conditional rule.
4. **Anonymized demo emails:** Demonstration emails are based on real public seller posts from online negotiation threads, with names and personal identifiers anonymized.
5. **Currency:** Built and tested with USD and US sandbox accounts. The seller's currency comes from `seller.json`; the reader recognises USD, EUR and GBP, and a deal in another currency stops with a question.
6. **The buyer's-why note is a guess:** When the buyer gives no reason, its guesses can be obvious ("wants to protect cash flow"). It never changes a number, a choice or the reply; the seller decides whether it matters.

## Licence

Licence: see LICENSE (chosen by the owner).
