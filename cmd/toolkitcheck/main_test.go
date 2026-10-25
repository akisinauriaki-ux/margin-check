package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"margincheck/internal/paypal"
)

func TestToolkitCheckMissingEnvPrintsExactMessageAndStops(t *testing.T) {
	for _, k := range []string{
		"PAYPAL_CLIENT_ID",
		"PAYPAL_CLIENT_SECRET",
		"PAYPAL_API_BASE",
		"PAYPAL_SANDBOX_SELLER_EMAIL",
		"PAYPAL_SANDBOX_BUYER_EMAIL",
	} {
		t.Setenv(k, "")
	}

	tmp := t.TempDir()
	reportPath := filepath.Join(tmp, "reports", "day1-toolkit.txt")

	var out bytes.Buffer
	code := runToolkitProof(context.Background(), filepath.Join(tmp, "missing.env"), reportPath, nil, nil, &out)
	if code != 0 {
		t.Fatalf("expected clean exit code 0 when .env is missing, got %d", code)
	}
	if strings.TrimSpace(out.String()) != paypal.MissingEnvMessage {
		t.Fatalf("got %q, want %q", strings.TrimSpace(out.String()), paypal.MissingEnvMessage)
	}
}
