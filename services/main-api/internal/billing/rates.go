// Package billing wires the running-instance metering. The Worker scans
// running instances on each tick, charges the user's credit balance for the
// minute bucket they're in, and sweeps users whose balance has gone ≤ 0 by
// dispatching DestroyInstance through the agent registry.
package billing

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// RateTable maps a GPU count to a per-minute charge in milli-원.
type RateTable struct {
	Rates map[int32]int64
}

type ratesFile struct {
	GpuRatesMilliPerMinute map[int32]int64 `yaml:"gpu_rates_milli_per_minute"`
}

// LoadRates reads and validates a rates.yaml. The keys are GPU counts (must
// be ≥ 0); values are milli-원 charged per minute the instance runs.
func LoadRates(path string) (*RateTable, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("read rates: %w", err)
	}
	var f ratesFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse rates: %w", err)
	}
	if len(f.GpuRatesMilliPerMinute) == 0 {
		return nil, errors.New("rates: gpu_rates_milli_per_minute must have at least one entry")
	}
	for gpu, rate := range f.GpuRatesMilliPerMinute {
		if gpu < 0 {
			return nil, fmt.Errorf("rates: gpu count %d is negative", gpu)
		}
		if rate < 0 {
			return nil, fmt.Errorf("rates: rate for gpu=%d is negative", gpu)
		}
	}
	return &RateTable{Rates: f.GpuRatesMilliPerMinute}, nil
}

// MilliPerMinute returns the rate for the given GPU count. Missing entries
// fall back to 0 — the operator should keep the table aligned with the
// supported gpu_count choices, but a 0 charge is safer than crashing.
func (t *RateTable) MilliPerMinute(gpuCount int32) int64 {
	if t == nil {
		return 0
	}
	return t.Rates[gpuCount]
}
