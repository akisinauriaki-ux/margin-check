package main

type Mode string

const (
	ModeReplay Mode = "replay"
	ModeLive   Mode = "live"
)

type ExamReport struct {
	WrongButSure      int
	EmailsRight       int
	EmailsTotal       int
	FieldsRight       int
	FieldsTotal       int
	InjectionsIgnored int
	InjectionsTotal   int
	MustAskAsked      int
	MustAskTotal      int
	UnnecessaryAsks   int
	MutationsPassed   int
	MutationsTotal    int
	UnavailableChecks int

	ProfitRight int
	ProfitTotal int
	LevelRight  int
	LevelTotal  int
	FalseAlarms int

	AvgCalls   float64
	MaxCalls   int
	AvgTokens  float64
	MaxTokens  int
	AvgSeconds float64
	MaxSeconds float64
}

type SafetyResults struct {
	TamperedTried           int
	TamperedBlocked         int
	PayPalCallsDuringBlock  int
	PlanChangedTried        int
	PlanChangedBlocked      int
	RepeatDraftsCreated     int
	RepeatDuplicates        int
	ReplyAttacksTried       int
	ReplyAttacksRejected    int
	WrongAIPlanTried        int
	WrongAIPlanBlocked      int
}

func (s SafetyResults) AllPassed() bool {
	return false
}

type Baseline struct {
	PromptVersion string   `json:"prompt_version"`
	Models        []string `json:"models"`
}

type Scorecard struct {
	Pass bool
}

type PretendPayPal struct {
	calls int
}

func NewPretendPayPal() *PretendPayPal {
	return &PretendPayPal{}
}

func (p *PretendPayPal) RecordCall() {
	p.calls++
}

func defaultExecute(tools *PretendPayPal, name string, tamperedPlan bool) (bool, error) {
	return false, nil
}

func ParseExamReport(data []byte) (ExamReport, error) {
	return ExamReport{}, nil
}

func RunSafetyChecks(tools *PretendPayPal, exec func(*PretendPayPal, string, bool) (bool, error)) SafetyResults {
	return SafetyResults{}
}

func RunSafetyChecksWithExecutor(tools *PretendPayPal, exec func(*PretendPayPal, string, bool) (bool, error)) SafetyResults {
	return SafetyResults{}
}

func CheckRegression(base *Baseline, report ExamReport, safety SafetyResults) []string {
	return nil
}

func BuildScorecard(r ExamReport, s SafetyResults, b *Baseline, m Mode, paypalRun bool, regs []string) Scorecard {
	return Scorecard{Pass: false}
}

func FormatScorecard(sc Scorecard) string {
	return "STUB SCORECARD"
}

func main() {}
