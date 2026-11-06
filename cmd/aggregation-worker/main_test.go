package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// B2：收到关闭信号后在途轮次超过预算时必须点名 aggregation-loop 并返回错误，
// 调用方据此非零退出；不能把“轮次还在跑”写成正常关闭。
func TestRunLoopWithDrainNamesStuckAggregationLoop(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	running := make(chan struct{}, 1)
	pass := aggregation.Pass{
		SyncSources: func(context.Context) error {
			select {
			case running <- struct{}{}:
			default:
			}
			<-release
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-running
		cancel()
	}()

	startedAt := time.Now()
	err := runLoopWithDrain(ctx, time.Hour, pass, 50*time.Millisecond, discardLogger())
	if err == nil {
		t.Fatal("runLoopWithDrain() = nil, want budget error naming aggregation-loop")
	}
	if !strings.Contains(err.Error(), "aggregation-loop") {
		t.Fatalf("error %q must name aggregation-loop", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
		t.Fatalf("drain overshot its budget: %v", elapsed)
	}
}

// 正常路径：信号到达时没有在途轮次，join 必须立即无错返回且不等待满预算。
func TestRunLoopWithDrainExitsCleanlyWhenIdle(t *testing.T) {
	pass := aggregation.Pass{SyncSources: func(context.Context) error { return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	startedAt := time.Now()
	if err := runLoopWithDrain(ctx, time.Millisecond, pass, 5*time.Second, discardLogger()); err != nil {
		t.Fatalf("runLoopWithDrain() = %v, want nil", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("drain waited %v after an idle loop", elapsed)
	}
}
