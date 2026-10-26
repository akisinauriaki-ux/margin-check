package why

import (
	"context"

	"margincheck/internal/gemini"
)

// PromptVersion names the instructions; bump it whenever they change.
const PromptVersion = "why-v1"

// MaxGuesses is how many guesses the seller sees at most.
const MaxGuesses = 2

// Generator is the model client (internal/gemini satisfies it).
type Generator interface {
	Generate(ctx context.Context, req gemini.Request) (gemini.Response, error)
}

// Config configures one Read call.
type Config struct {
	Client Generator
	Model  string
}

// Guess is one possible reason behind what the buyer asks for, with the buyer's words it rests on.
type Guess struct {
	Interest string `json:"interest"`
	Quote    string `json:"quote"`
}

// Note is what the seller sees. No guesses means the AI couldn't tell from the buyer's words.
type Note struct {
	Guesses  []Guess  `json:"guesses,omitempty"`
	Problems []string `json:"problems,omitempty"` // why AI guesses were not shown
	Model    string   `json:"model,omitempty"`
	Tokens   int      `json:"tokens"`
	Mode     string   `json:"mode,omitempty"` // live or replayed
}

// Read asks the AI why the buyer may be asking, and keeps only the guesses that pass Check.
func Read(ctx context.Context, cfg Config, thread string) Note { return Note{} }

// Check lists every reason a guess can't be shown. buyerWords is what the buyer wrote.
func Check(g Guess, buyerWords string) []string { return nil }

// BuyerWords returns the buyer's part of a thread.
func BuyerWords(thread string) string { return "" }
