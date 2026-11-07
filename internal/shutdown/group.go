// Package shutdown 提供进程退出的有界 join（B2）。
//
// 长驻进程里的后台循环（聚合轮次、告警评估、事务内审计 outbox 投递……）在收到
// SIGTERM 后必须先停止再退出：否则容器要么带着未完成的写事务被杀，要么把
// “还在跑”伪装成“已退出”。本包把每个后台循环登记为命名组件，关闭时在统一预算内
// 逐一等待，超预算则点名仍未退出的组件并让调用方非零退出。
//
// 取舍：没有预算的无限等待会把编排层的终止宽限变成硬杀；没有 join 的立即退出会
// 截断在途任务。因此这里只做“有界等待 + 点名”，不替调用方决定补偿动作。
package shutdown

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EnvDrainBudget 是关闭预算的覆盖变量，单位为秒。
const EnvDrainBudget = "DATA_ANALYSIS_SHUTDOWN_DRAIN_SECONDS"

// DefaultBudget 是未配置覆盖变量时的关闭预算。
// 20s 短于 docker/k8s 默认 30s 的终止宽限，留出“点名未退出组件后非零退出”的余量。
const DefaultBudget = 20 * time.Second

// BudgetFromEnv 解析关闭预算。
// 参数：getenv 为环境读取函数（生产为 os.Getenv，测试可替换）。
// 返回值：预算时长；未配置取 DefaultBudget；非正整数或非整数秒返回错误，
// 由调用方日志并非零退出（fail-closed，避免配置错误静默退回默认值）。
func BudgetFromEnv(getenv func(string) string) (time.Duration, error) {
	if getenv == nil {
		return DefaultBudget, nil
	}
	raw := strings.TrimSpace(getenv(EnvDrainBudget))
	if raw == "" {
		return DefaultBudget, nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer number of seconds", EnvDrainBudget)
	}
	return time.Duration(seconds) * time.Second, nil
}

// component 是一个已登记的后台组件：done 在其退出后关闭。
type component struct {
	name string
	done <-chan struct{}
}

// Group 收集需要在进程退出前 join 的命名后台组件。
// 方法可并发调用（登记可与关闭并行），组件退出顺序不作要求。
type Group struct {
	mu     sync.Mutex
	joined []component
}

// New 创建空的后台组件组。
func New() *Group {
	return &Group{}
}

// Register 登记一个后台组件。
// 参数：name 为日志/错误里点名的组件名；done 在该组件退出后关闭（不得为 nil）。
// 返回值：无。重复登记同一 name 会被保留为独立条目，Pending 输出时按名去重。
func (g *Group) Register(name string, done <-chan struct{}) {
	if g == nil || done == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.joined = append(g.joined, component{name: name, done: done})
}

// Run 在独立 goroutine 中执行 fn 并登记为组件。
// 参数：name 为组件名；fn 为后台循环（应在 ctx 取消后尽快返回）。
// 返回值：恰好收到一次 fn 结果的通道；组件退出后其值已就绪，
// 因此 Join 返回后再读取不会阻塞。仅当 Join 超预算（组件仍在跑）时不应读取该通道。
func (g *Group) Run(name string, fn func() error) <-chan error {
	result := make(chan error, 1)
	done := make(chan struct{})
	g.Register(name, done)
	go func() {
		// 先投递结果再关闭 done：Join 返回时结果一定已在通道里。
		defer close(done)
		result <- run(fn)
	}()
	return result
}

// run 执行组件函数；nil 函数视为“调用方配置缺失”，返回错误而不是静默成功。
func run(fn func() error) error {
	if fn == nil {
		return errors.New("shutdown: nil component function")
	}
	return fn()
}

// Join 在预算内等待全部已登记组件退出。
// 参数：budget 为最长等待时间；<=0 表示只做即时检查、不等待。
// 返回值：全部按时退出为 nil；否则返回点名未退出组件的错误，调用方必须非零退出
// （不能把“组件没退出”写成正常关闭）。
func (g *Group) Join(budget time.Duration) error {
	if g == nil {
		return nil
	}
	deadline := time.Now().Add(budget)
	g.mu.Lock()
	components := make([]component, len(g.joined))
	copy(components, g.joined)
	g.mu.Unlock()
	for _, item := range components {
		if budget <= 0 {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		timer := time.NewTimer(remaining)
		select {
		case <-item.done:
			timer.Stop()
		case <-timer.C:
		}
	}
	pending := g.Pending()
	if len(pending) == 0 {
		return nil
	}
	return fmt.Errorf("shutdown budget %s exceeded; still running: %s", budget, strings.Join(pending, ", "))
}

// Pending 返回仍未退出的组件名（去重并按字典序排序，便于日志与断言稳定）。
func (g *Group) Pending() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	seen := make(map[string]struct{}, len(g.joined))
	pending := make([]string, 0, len(g.joined))
	for _, item := range g.joined {
		select {
		case <-item.done:
			continue
		default:
		}
		if _, exists := seen[item.name]; exists {
			continue
		}
		seen[item.name] = struct{}{}
		pending = append(pending, item.name)
	}
	sort.Strings(pending)
	return pending
}
