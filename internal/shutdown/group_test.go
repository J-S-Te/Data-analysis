package shutdown

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// B2：预算默认 20s，env 覆盖；非法值必须失败关闭，不能静默退回默认值。
func TestBudgetFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", value: "", want: DefaultBudget},
		{name: "override", value: "5", want: 5 * time.Second},
		{name: "trimmed", value: " 30 ", want: 30 * time.Second},
		{name: "zero", value: "0", wantErr: true},
		{name: "negative", value: "-3", wantErr: true},
		{name: "fraction", value: "1.5", wantErr: true},
		{name: "text", value: "twenty", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BudgetFromEnv(func(key string) string {
				if key != EnvDrainBudget {
					t.Fatalf("unexpected env key %q", key)
				}
				return tc.value
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("BudgetFromEnv(%q) = %v, want error", tc.value, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("BudgetFromEnv(%q) = %v, %v; want %v", tc.value, got, err, tc.want)
			}
		})
	}
}

// 正常路径：组件自行退出时 Join 必须无错返回，并且不消耗满预算。
func TestJoinReturnsNilWhenComponentsExit(t *testing.T) {
	group := New()
	released := make(chan struct{})
	results := group.Run("outbox-dispatcher", func() error {
		<-released
		return nil
	})
	close(released)
	startedAt := time.Now()
	if err := group.Join(5 * time.Second); err != nil {
		t.Fatalf("Join() = %v, want nil", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("Join() waited %v after components exited", elapsed)
	}
	if pending := group.Pending(); len(pending) != 0 {
		t.Fatalf("Pending() = %v, want none", pending)
	}
	if err := <-results; err != nil {
		t.Fatalf("component result = %v, want nil", err)
	}
}

// 超预算：Join 必须点名仍未退出的组件而非假装成功，并如实返回预算耗时错误。
func TestJoinTimesOutAndNamesStuckComponents(t *testing.T) {
	group := New()
	stubborn := make(chan struct{})
	quiet := make(chan struct{})
	close(quiet)
	group.Register("aggregation-loop", stubborn)
	group.Register("alert-worker-loop", stubborn)
	group.Register("outbox-dispatcher", quiet)
	t.Cleanup(func() { close(stubborn) })

	startedAt := time.Now()
	err := group.Join(30 * time.Millisecond)
	if err == nil {
		t.Fatal("Join() = nil, want budget error naming stuck components")
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("Join() overshot its budget: %v", elapsed)
	}
	message := err.Error()
	for _, name := range []string{"aggregation-loop", "alert-worker-loop"} {
		if !strings.Contains(message, name) {
			t.Fatalf("Join() error %q must name %q", message, name)
		}
	}
	if strings.Contains(message, "outbox-dispatcher") {
		t.Fatalf("Join() error %q must not name components that exited", message)
	}
	if pending := group.Pending(); len(pending) != 2 || pending[0] != "aggregation-loop" || pending[1] != "alert-worker-loop" {
		t.Fatalf("Pending() = %v, want sorted stuck components", pending)
	}
}

// 零预算只做即时检查，不等待：调用方已经用满公共 deadline 时仍要能点名。
func TestJoinWithZeroBudgetReportsPendingImmediately(t *testing.T) {
	group := New()
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	group.Register("outbox-dispatcher", stuck)
	startedAt := time.Now()
	if err := group.Join(0); err == nil || !strings.Contains(err.Error(), "outbox-dispatcher") {
		t.Fatalf("Join(0) = %v, want error naming pending component", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("Join(0) waited %v", elapsed)
	}
}

// 循环错误必须原样交回调用方，不能被 join 吞掉。
func TestRunSurfacesComponentError(t *testing.T) {
	group := New()
	want := errors.New("aggregation pass failed")
	results := group.Run("aggregation-loop", func() error { return want })
	if err := group.Join(5 * time.Second); err != nil {
		t.Fatalf("Join() = %v, want nil", err)
	}
	if err := <-results; !errors.Is(err, want) {
		t.Fatalf("component result = %v, want %v", err, want)
	}
}
