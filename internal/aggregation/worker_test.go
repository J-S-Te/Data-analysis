package aggregation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestPassRunOnceExecutesEveryConfiguredChannel(t *testing.T) {
	var calls []string
	pass := Pass{
		SyncSources: func(context.Context) error {
			calls = append(calls, "sources")
			return errors.New("source unavailable")
		},
		SyncContractDashboard: func(context.Context) error {
			calls = append(calls, "contract")
			return nil
		},
		SyncProjectDashboard: func(context.Context) error {
			calls = append(calls, "project")
			return errors.New("project unavailable")
		},
	}

	err := pass.RunOnce(context.Background())
	if got := strings.Join(calls, ","); got != "sources,contract,project" {
		t.Fatalf("execution order = %q, want sources,contract,project", got)
	}
	if err == nil {
		t.Fatal("RunOnce() error = nil, want joined errors")
	}
	for _, want := range []string{"database sources", "project dashboard API"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("RunOnce() error = %q, want substring %q", err, want)
		}
	}
}

func TestPassConfigured(t *testing.T) {
	if (Pass{}).Configured() {
		t.Fatal("empty Pass.Configured() = true")
	}
	if !(Pass{SyncSources: func(context.Context) error { return nil }}).Configured() {
		t.Fatal("configured Pass.Configured() = false")
	}
}

func TestAggregationRunLoopRejectsNonPositiveInterval(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := RunLoop(context.Background(), 0, Pass{}, logger); err == nil {
		t.Fatal("RunLoop() error = nil, want invalid interval error")
	}
}
