package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunLoopWithDrainKeepsRunningBeforeCancellation 验证健康循环跨越关闭预算仍持续执行。
// 参数：t 为测试上下文，通道仅在内存中计数，不访问业务依赖。
// 返回值：无；提前退出、停止执行或取消后未及时结束时失败。
func TestRunLoopWithDrainKeepsRunningBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rounds atomic.Int64
	pass := aggregation.Pass{SyncSources: func(context.Context) error {
		rounds.Add(1)
		return nil
	}}
	result := make(chan error, 1)
	go func() {
		result <- runLoopWithDrain(ctx, time.Millisecond, pass, 20*time.Millisecond, discardLogger())
	}()
	select {
	case err := <-result:
		t.Fatalf("healthy loop exited before cancellation: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	if rounds.Load() < 2 {
		t.Fatal("healthy loop did not execute multiple rounds")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("canceled healthy loop returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy loop did not exit after cancellation")
	}
}

// TestRunLoopWithDrainReturnsEarlyLoopError 验证循环自身报错不必等待外部关闭信号。
// 参数：t 为测试上下文，零间隔触发真实循环的启动校验错误。
// 返回值：无；错误丢失或入口仍等待取消时失败。
func TestRunLoopWithDrainReturnsEarlyLoopError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runLoopWithDrain(ctx, 0, aggregation.Pass{}, time.Second, discardLogger())
	}()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "interval must be positive") {
			t.Fatalf("early loop error = %v, want invalid interval", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("loop error remained hidden while waiting for cancellation")
	}
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
