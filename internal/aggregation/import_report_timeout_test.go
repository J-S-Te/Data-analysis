package aggregation

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestImportReportSourceHandshakeHonorsContext 验证接受TCP但不握手的源库不会阻塞本轮同步。
// 参数：t为隔离回环网络测试上下文，不使用任何真实业务DSN。
// 返回值：无；超时或取消后仍卡在源初始化时失败。
func TestImportReportSourceHandshakeHonorsContext(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				connection, e := listener.Accept()
				if e == nil {
					accepted <- connection
				}
			}()
			runner := reportDB(t)
			runner.sources = map[string]string{"contract_management": "qa:qa@tcp(" + listener.Addr().String() + ")/contract_test_fake"}
			var ctx context.Context
			var cancel context.CancelFunc
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			result := make(chan string, 1)
			go func() { _, code := runner.readImportSourceFacts(ctx, reportFixture()); result <- code }()
			var connection net.Conn
			select {
			case connection = <-accepted:
			case <-time.After(time.Second):
				t.Fatal("未建立隔离源连接")
			}
			defer connection.Close()
			if mode == "cancel" {
				cancel()
			}
			select {
			case code := <-result:
				if code != "SOURCE_UNAVAILABLE" {
					t.Fatalf("错误状态=%s", code)
				}
			case <-time.After(500 * time.Millisecond):
				t.Error("源初始化未遵守本轮上下文")
				connection.Close()
				listener.Close()
				select {
				case <-result:
				case <-time.After(time.Second):
					t.Error("关闭隔离连接后源初始化仍未退出")
				}
			}
		})
	}
}
