package config

import (
	"testing"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input   string
		wantMS  int
		wantErr bool
	}{
		{"30s", 30000, false},
		{"5m", 300000, false},
		{"1h", 3600000, false},
		{"1h30m", 5400000, false},
		{"1m30s", 90000, false},
		{"500ms", 500, false},
		{"5000", 5000, false},
		{"", 0, true},
		{"abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseDuration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.wantMS {
				t.Errorf("parseDuration(%q) = %d, want %d", tt.input, got, tt.wantMS)
			}
		})
	}
}

func TestPollingIntervalFromHumanString(t *testing.T) {
	yaml := `---
polling:
  interval: 5m
---
Test prompt`

	wf, err := ParseWorkflow([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseWorkflow failed: %v", err)
	}

	if wf.Config.Polling.IntervalMS != 300000 {
		t.Errorf("IntervalMS = %d, want 300000", wf.Config.Polling.IntervalMS)
	}
}

func TestPollingIntervalMSFallback(t *testing.T) {
	yaml := `---
polling:
  interval_ms: 60000
---
Test prompt`

	wf, err := ParseWorkflow([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseWorkflow failed: %v", err)
	}

	if wf.Config.Polling.IntervalMS != 60000 {
		t.Errorf("IntervalMS = %d, want 60000", wf.Config.Polling.IntervalMS)
	}
}

func TestPollingIntervalOverridesIntervalMS(t *testing.T) {
	yaml := `---
polling:
  interval: 10s
  interval_ms: 60000
---
Test prompt`

	wf, err := ParseWorkflow([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseWorkflow failed: %v", err)
	}

	// interval should take precedence over interval_ms
	if wf.Config.Polling.IntervalMS != 10000 {
		t.Errorf("IntervalMS = %d, want 10000", wf.Config.Polling.IntervalMS)
	}
}
