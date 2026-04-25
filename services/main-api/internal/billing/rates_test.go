package billing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRates_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rates.yaml")
	if err := os.WriteFile(path, []byte("gpu_rates_milli_per_minute:\n  0: 100\n  1: 16667\n  2: 33333\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := LoadRates(path)
	if err != nil {
		t.Fatal(err)
	}
	if rt.MilliPerMinute(1) != 16667 {
		t.Fatalf("gpu=1: %d", rt.MilliPerMinute(1))
	}
	if rt.MilliPerMinute(8) != 0 {
		t.Fatal("missing key should be 0")
	}
}

func TestLoadRates_RejectsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rates.yaml")
	if err := os.WriteFile(path, []byte("gpu_rates_milli_per_minute: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRates(path); err == nil {
		t.Fatal("expected error for empty rate table")
	}
}

func TestLoadRates_RejectsNegativeRate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rates.yaml")
	if err := os.WriteFile(path, []byte("gpu_rates_milli_per_minute:\n  1: -100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRates(path); err == nil {
		t.Fatal("expected error for negative rate")
	}
}

func TestRateTable_NilSafe(t *testing.T) {
	t.Parallel()
	var rt *RateTable
	if rt.MilliPerMinute(1) != 0 {
		t.Fatal("nil table should yield 0")
	}
}
