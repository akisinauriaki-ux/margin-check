# How margin-check is engineered

margin-check lets an AI agent prepare money actions: PayPal invoice drafts built from a negotiation email. That is exactly where AI systems fail badly. They believe a tricked email, they act on a guess, or they repeat an action. So the design starts from one rule: **the AI may propose; only code decides, and only the seller approves.**

Every principle below is one we took from the engineering literature and then checked against our own code. Each one points to the test, file or output that shows it. The tags (`[E-NN]`) are checked by script, then read by hand.

## 1. The agent can't act on its own

**The agent has no free hand: tool choice is separated from execution, and every action passes a code check.**
- **The books:** *Building Agentic AI Systems* (decouple the choice of a tool from its execution); *AI Engineering* by Chip Huyen (validate a plan before running it); the OWASP Agentic Top 10 (complete mediation).
- **Here:** the AI planner may suggest the PayPal calls. Code rebuilds the calls the accepted choice requires, and an interceptor compares the two field by field, both when the plan is made and again just before it runs [E-01].
- **Two tools only,** with the narrowest permissions. PayPal's toolkit starts limited to creating invoices [E-02]. The toolkit's process receives only a short-lived sandbox token, never the client secret or the AI key [E-03].

**A human approves the exact action, not a description of it.**
- **The books:** *Agentic Design Patterns* (a human in the loop for financial actions).
- **Here:** approval is bound to a SHA-256 fingerprint of the exact calls, their arguments and their order. A plan changed after approval never runs [E-04].
- **The Approve button names the action in the plan's own terms,** for example "Create the PayPal draft: 30 boxes at $40.00 each, free shipping, due on receipt" [E-05]. Two books shaped this:
  - *Strategic Writing for UX* warns that vague action labels let people commit without knowing what will happen;
  - *Decision Making Under Uncertainty* records "automation complacency", where people approve automated recommendations without reading them.

**Not autonomous, on purpose.**
- **The books:** *Designing Large Language Model Applications* and *AI Agents in Action* both advise against fully autonomous agents in production.
- **Here:** margin-check is a fixed pipeline with a defined end. The agent can't loop, can't choose new tools, and can't send anything: only drafts exist.

## 2. The AI proposes; code decides (LLM-Modulo)

*LLMs Can't Plan, But Can Help Planning in LLM-Modulo Frameworks* describes the pattern we follow: the model generates, and verifiers it can't argue with decide. Here, a value from the email is marked **sure** only when all three of these hold:
- two different models read the same value;
- the exact sentence it came from is found in the email;
- code parsed it.

The model's own "confidence" is never used. The headline metric is therefore **wrong but marked sure**, which is 0 on the exam [E-06].

### Defence in depth
*The Design of Everyday Things* calls this the Swiss-cheese model: layers, so that one slip is caught by the next. *Site Reliability Engineering* gives the same advice. Seven layers stand between an email and PayPal:

| Layer | What it stops | Its proof |
|---|---|---|
| 1. Exact quote match | A number the email doesn't contain | [E-07] |
| 2. Two readings must agree | One model's misreading | [E-08] |
| 3. Profit maths in code, in whole cents | AI arithmetic | [E-09] |
| 4. Interceptor at plan time | A wrong AI plan | [E-10] |
| 5. Fingerprinted approval | A plan changed after the seller approved it | [E-04] |
| 6. Interceptor again at run time | A plan that was approved but is wrong | [E-01] |
| 7. Read-back from PayPal | PayPal holding something other than what was approved | [E-11] |

## 3. Evaluation came before the system

- **Evaluation-driven development** (Huyen, *AI Engineering*): the 20-email exam and its answer key entered the repository on 25 Oct, a day before the AI reader existed on 26 Oct [E-12]. The reader was built to pass a test written before it.
- **Mutation tests:** six exam emails with one fact changed. Exactly that answer must change, and it does, 6 of 6 [E-13].
- **A held-out set:** the reviewer keeps a separate set of unseen emails outside this repository and re-tests on it, so the exam can't simply be memorised. This is our statement; by design it can't be checked from the repository.
- **A quality gate that can't silently drop.** *Building Evolutionary Architectures* calls an automated check of an architectural property a "fitness function". `go run ./cmd/proof`:
  - compares every number with a recorded baseline [E-14];
  - fails if the safety checks are sabotaged. With the interceptor switched off, it reports 0 of 12 tampered plans blocked and FAILS [E-15].
- **Deterministic replay** (Huyen, on caching model responses): the recorded AI answers ship in the repository, so `proof` reruns the whole exam with no key and no AI call. In replay mode a missing answer is a failure, never a live call [E-16].

## 4. Money is handled like money

- **Whole cents, never floating point,** with each line rounded half away from zero. The profit engine is checked against hand-calculated answers [E-17].
- **Duplicates are suppressed end to end.** *Designing Data-Intensive Applications* is explicit: the application itself must suppress duplicates, whatever the layers below promise.
  - We tested PayPal's own request-id in the sandbox. It was ignored on invoice create: two creates made two invoices.
  - So each deal and choice gets one fixed invoice number. The agent looks for it before creating, and again after any failure. A repeat run finds the same draft [E-18].
- **Change money only through an approved procedure.** *Security Engineering* describes the Clark-Wilson model: data changes only through approved transformation procedures. The only path to PayPal is the approved, fingerprinted plan.
- **Common-sense checks before acting** (*The Design of Everyday Things*):
  - a choice that would lose money is never offered [E-19];
  - if the buyer accepts a choice but states a different price, payment term, discount or shipping, nothing is planned, and the seller decides [E-20].
- **An append-only decision log.** *Designing Data-Intensive Applications* and *Clean Architecture* both favour append-only records over overwriting; *Security Engineering* calls bookkeeping the main defence against insider fraud.
  - Every step of a deal adds a line to its log, and no line is ever rewritten [E-21].
  - The approval is written before PayPal is called. If the log can't be written, nothing runs [E-22].

## 5. Decisions and failures are written down

- **Decisions with their trade-offs.** *Fundamentals of Software Architecture*: everything is a trade-off, and the why matters more than the how. Each important choice has a record with the options, what was good and bad about each, and the evidence. Summaries are in [DECISIONS.md](DECISIONS.md).
- **Failures, without blame.** *Software Engineering at Google* and *Site Reliability Engineering* both describe blameless postmortems. The README's failure register lists what went wrong during the build and the test that now guards each one.
- **Independent review.** Two review rounds by a second reviewer found real defects after the build "worked": a decimal tail read as a whole number, an invoice search that could return a different invoice, and a buyer's changed price that still became a plan. Each was fixed with a failing test committed first. The git history shows the pairs.

## What we deliberately did not build

- **Multi-party approval:** there is one seller and one approver, by design.
- **Multiple agents:** the MAST study of multi-agent failures found that more agents talking to each other fail more often. One pipeline is simpler to verify.
- **An AI judging the AI:** our checks are exact code, which is stronger for numbers.
- **Prompts as files for the reply writer and planner.** *AI Engineering* recommends keeping prompts as versioned files. The reader's prompt is a file in `prompts/`. The reply writer's and planner's prompts are versioned constants inside the code, and their version is part of the replay key. Moving them would invalidate the recorded answers, so we left them and say so here.
