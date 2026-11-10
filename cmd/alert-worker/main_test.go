package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/alertworker"
)

type loopTestStore struct {
	alertworker.Store
	list func(context.Context) ([]alertworker.Rule, error)
}

// ListEnabledRules 提供内存规则读取边界，保留真实告警循环和轮次执行。
// 参数：ctx 为当前评估轮次的取消上下文。
// 返回值：测试场景的规则列表和读取错误，不执行数据库访问。
func (store loopTestStore) ListEnabledRules(ctx context.Context) ([]alertworker.Rule, error) {
	return store.list(ctx)
}

// discardLogger 创建不输出日志的测试日志器。
// 参数：无。
// 返回值：供真实循环使用的结构化日志器。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunLoopWithDrainKeepsRunningBeforeCancellation 验证健康告警循环跨越关闭预算仍执行。
// 参数：t 为测试上下文，内存规则列表为空，不产生业务写入。
// 返回值：无；提前退出、停止执行或取消后未及时结束时失败。
func TestRunLoopWithDrainKeepsRunningBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rounds atomic.Int64
	worker := alertworker.New(loopTestStore{list: func(context.Context) ([]alertworker.Rule, error) {
		rounds.Add(1)
		return nil, nil
	}})
	result := make(chan error, 1)
	go func() {
		result <- runLoopWithDrain(ctx, time.Millisecond, worker, 20*time.Millisecond, discardLogger())
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

// TestRunLoopWithDrainReturnsEarlyLoopError 验证告警循环自身报错不必等待外部关闭信号。
// 参数：t 为测试上下文，零间隔在业务评估前触发真实循环校验错误。
// 返回值：无；错误丢失或入口仍等待取消时失败。
func TestRunLoopWithDrainReturnsEarlyLoopError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runLoopWithDrain(ctx, 0, nil, time.Second, discardLogger())
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

// TestRunLoopWithDrainNamesStuckAlertLoop 验证取消后卡住的评估必须有界失败并点名。
// 参数：t 为测试上下文，内存读取故意忽略取消并在测试清理时释放。
// 返回值：无；未报告关闭超时、组件名丢失或超出容差时失败。
func TestRunLoopWithDrainNamesStuckAlertLoop(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	running := make(chan struct{}, 1)
	worker := alertworker.New(loopTestStore{list: func(context.Context) ([]alertworker.Rule, error) {
		select {
		case running <- struct{}{}:
		default:
		}
		<-release
		return nil, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-running
		cancel()
	}()
	startedAt := time.Now()
	err := runLoopWithDrain(ctx, time.Hour, worker, 50*time.Millisecond, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "alert-worker-loop") {
		t.Fatalf("drain error = %v, want timeout naming alert-worker-loop", err)
	}
	if time.Since(startedAt) > 3*time.Second {
		t.Fatal("alert drain exceeded its shutdown budget tolerance")
	}
}
