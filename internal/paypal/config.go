package paypal

import (
	"bufio"
	"errors"
	"os"
	"strings"
)

// MissingEnvMessage is the exact message printed when .env is missing or incomplete.
const MissingEnvMessage = "fill in .env (see .env.example)"

// SandboxConfig holds the non-printed sandbox credentials and test account emails from the environment.
type SandboxConfig struct {
	ClientID     string
	ClientSecret string
	APIBase      string
	SellerEmail  string
	BuyerEmail   string
}

var requiredPayPalEnvKeys = []string{
	"PAYPAL_CLIENT_ID",
	"PAYPAL_CLIENT_SECRET",
	"PAYPAL_API_BASE",
	"PAYPAL_SANDBOX_SELLER_EMAIL",
	"PAYPAL_SANDBOX_BUYER_EMAIL",
}

// LoadSandboxConfig loads required PayPal sandbox settings from process environment variables,
// falling back to reading KEY=VALUE entries from dotenvPath if present.
// It returns ready=false (with err=nil) when dotenvPath is missing or any required variable is blank.
// Secret values are never included in any error.
func LoadSandboxConfig(dotenvPath string) (cfg *SandboxConfig, ready bool, err error) {
	fileVals := map[string]string{}
	if strings.TrimSpace(dotenvPath) != "" {
		info, statErr := os.Stat(dotenvPath)
		if statErr == nil && !info.IsDir() {
			parsed, parseErr := parseDotEnvFile(dotenvPath)
			if parseErr != nil {
				return nil, false, errors.New("paypal: unable to read .env file")
			}
			fileVals = parsed
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, false, errors.New("paypal: unable to stat .env file")
		}
	}

	lookup := func(key string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return strings.TrimSpace(fileVals[key])
	}

	for _, key := range requiredPayPalEnvKeys {
		if lookup(key) == "" {
			return nil, false, nil
		}
	}

	return &SandboxConfig{
		ClientID:     lookup("PAYPAL_CLIENT_ID"),
		ClientSecret: lookup("PAYPAL_CLIENT_SECRET"),
		APIBase:      lookup("PAYPAL_API_BASE"),
		SellerEmail:  lookup("PAYPAL_SANDBOX_SELLER_EMAIL"),
		BuyerEmail:   lookup("PAYPAL_SANDBOX_BUYER_EMAIL"),
	}, true, nil
}

func parseDotEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("dotenv open failed")
	}
	defer f.Close()

	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, errors.New("dotenv scan failed")
	}
	return out, nil
}
