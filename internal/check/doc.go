// Package check connects the reading stage (reader.CheckResult) to the
// profit engine (profit.Deal and profit.Evaluate).
//
// Core rule: Only sure, agreed values reach the profit engine. Any "ask" stops
// the check before any money figure is shown, and lists the questions (M6 for
// the seller's numbers, M4 for the deal's numbers).
package check
