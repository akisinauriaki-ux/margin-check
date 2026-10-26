package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	var (
		liveFlag          = flag.Bool("live", false, "Use live Gemini calls (needs GEMINI_API_KEY)")
		paypalFlag        = flag.Bool("paypal", false, "Run cmd/actcheck against PayPal sandbox (needs .env)")
		writeBaselineFlag = flag.Bool("write-baseline", false, "Rewrite proof-baseline.json with current numbers")
		dirFlag           = flag.String("dir", "testdata", "Test data directory for exam")
		baselineFlag      = flag.String("baseline-file", "proof-baseline.json", "Path to proof baseline file")
		modelsFlag        = flag.String("models", "gemini-3.5-flash-lite,gemini-3.1-flash-lite", "Models to test")
		promptFlag        = flag.String("prompt", "reader-v1", "Reader prompt version")
	)
	flag.Parse()

	mode := ModeReplay
	if *liveFlag {
		mode = ModeLive
		fmt.Println("Warning: about 350,000 tokens and several minutes on the free tier.")
	}

	tempReportDir, err := os.MkdirTemp("", "proof-exam-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp directory: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tempReportDir)

	examArgs := []string{
		"run", "./cmd/exam",
		"--dir", *dirFlag,
		"--models", *modelsFlag,
		"--prompt", *promptFlag,
		"--mutations",
		"--report-dir", tempReportDir,
	}

	cmd := exec.Command("go", examArgs...)
	cmd.Env = os.Environ()
	if mode == ModeReplay {
		cmd.Env = append(cmd.Env, "GEMINI_API_KEY=replay-only-no-calls")
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	examErr := cmd.Run()

	var examReport ExamReport
	var examFound bool

	entries, _ := os.ReadDir(tempReportDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "exam-") && strings.HasSuffix(entry.Name(), ".json") {
			dataPath := filepath.Join(tempReportDir, entry.Name())
			data, readErr := os.ReadFile(dataPath)
			if readErr == nil {
				if parsed, pErr := ParseExamReport(data); pErr == nil {
					examReport = parsed
					examFound = true
				}
			}
			break
		}
	}

	var examFailReasons []string
	if examErr != nil {
		errStr := stderrBuf.String()
		if strings.TrimSpace(errStr) == "" {
			errStr = stdoutBuf.String()
		}
		firstLine := strings.Split(strings.TrimSpace(errStr), "\n")[0]
		examFailReasons = append(examFailReasons, fmt.Sprintf("exam failed: %s", firstLine))
	}
	if !examFound {
		examFailReasons = append(examFailReasons, "exam JSON report was not generated")
	}

	var paypalPass bool
	var paypalLines []string
	if *paypalFlag {
		actCmd := exec.Command("go", "run", "./cmd/actcheck")
		actOut, actErr := actCmd.CombinedOutput()
		if actErr != nil {
			paypalLines = append(paypalLines, fmt.Sprintf("FAILED: %v", actErr))
		} else {
			paypalPass = true
			for _, l := range strings.Split(string(actOut), "\n") {
				trimmed := strings.TrimSpace(l)
				if trimmed != "" {
					paypalLines = append(paypalLines, trimmed)
				}
			}
		}
	}

	safety := RunSafetyChecks(NewPretendPayPal(), defaultExecute)

	var baseline *Baseline
	if b, bErr := LoadBaseline(*baselineFlag); bErr == nil {
		baseline = b
	}

	var regs []string
	if baseline != nil && examFound {
		regs = CheckRegression(baseline, examReport, safety)
	}

	if *writeBaselineFlag {
		modelsList := strings.Split(*modelsFlag, ",")
		for i := range modelsList {
			modelsList[i] = strings.TrimSpace(modelsList[i])
		}
		if err := WriteBaseline(*baselineFlag, *promptFlag, modelsList, examReport, safety); err != nil {
			fmt.Fprintf(os.Stderr, "error writing baseline: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Wrote new baseline to %s\n", *baselineFlag)
	}

	sc := BuildScorecard(examReport, safety, baseline, mode, *paypalFlag, paypalPass, paypalLines, regs)
	if *paypalFlag {
		sc.PayPalPass = paypalPass
		sc.PayPalLines = paypalLines
	}
	if len(examFailReasons) > 0 {
		sc.Pass = false
		sc.FailReasons = append(examFailReasons, sc.FailReasons...)
	}

	scorecardText := FormatScorecard(sc)
	fmt.Print(scorecardText)

	if err := os.MkdirAll("reports", 0o755); err == nil {
		_ = os.WriteFile(filepath.Join("reports", "proof-latest.txt"), []byte(scorecardText), 0o644)
	}

	if !sc.Pass {
		os.Exit(1)
	}
}
