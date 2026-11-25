package aggregation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSyncContractDashboardMapsVerifiedTenantSnapshot(t *testing.T) {
	t.Parallel()

	const tenantID = "tenant-1"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", request.Method)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer machine-token" {
			t.Errorf("Authorization = %q, want machine token", got)
		}
		if got := request.Header.Get("X-DA-Tenant-ID"); got != tenantID {
			t.Errorf("X-DA-Tenant-ID = %q, want %q", got, tenantID)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(response, `{
			"code":"OK",
			"data":{
				"tenant_id":"tenant-1",
				"total_amount_minor":880000,
				"currency_totals":[{"currency":"CNY","amount_minor":100000,"contracts":1},{"currency":"USD","amount_minor":780000,"contracts":7}],
				"total_contracts":8,
				"approval_contracts":2,
				"active_contracts":4,
				"expired_contracts":1
			}
		}`)
	}))
	defer server.Close()

	runner := &APISyncRunner{
		options: APISyncOptions{
			ContractInternalURL: server.URL,
			ContractCredential:  MachineCredential{ClientID: "contract-reader"},
			TenantID:            tenantID,
			HTTPTimeout:         time.Second,
		},
		tokens: map[string]cachedMachineToken{"contract-reader": {value: "machine-token", expiresAt: time.Now().Add(time.Hour)}},
	}

	var captured contractDashboardSnapshot
	err := runner.syncContractDashboard(context.Background(), func(_ context.Context, snapshot contractDashboardSnapshot) error {
		captured = snapshot
		return nil
	})
	if err != nil {
		t.Fatalf("syncContractDashboard() error = %v", err)
	}
	if captured.TenantID != tenantID || captured.TotalAmountMinor != 880000 ||
		captured.TotalContracts != 8 || captured.ApprovalContracts != 2 ||
		captured.ActiveContracts != 4 || captured.ExpiredContracts != 1 ||
		len(captured.CurrencyTotals) != 2 || captured.CurrencyTotals[0].Currency != "CNY" || captured.CurrencyTotals[1].Currency != "USD" {
		t.Fatalf("captured snapshot = %#v", captured)
	}
	if captured.SnapshotAt.IsZero() || captured.SnapshotAt.Location() != time.UTC {
		t.Fatalf("snapshot time = %v, want a UTC timestamp", captured.SnapshotAt)
	}
}

// TestSyncContractDashboardResponseLimits 核对大型合同响应可发布且非法响应不进入快照。
// 参数：t 为仅连接本机测试 HTTP 服务的测试上下文。
// 返回值：无。
func TestSyncContractDashboardResponseLimits(t *testing.T) {
	const prefix = `{"code":"OK","data":{"tenant_id":"tenant-1","contracts":[`
	const suffix = `],"contract_detail_limited":true,"total_contracts":205}}`
	detail := `{"description":"` + strings.Repeat("a", 13_000) + `"}`
	large := prefix + strings.Repeat(detail+",", 199) + detail + suffix
	for _, test := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "large", body: large},
		{name: "oversized_after_valid_json", body: large + strings.Repeat(" ", 16<<20), wantErr: "exceeds"},
		{name: "truncated", body: large[:len(large)-1], wantErr: "decode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			runner := NewAPISyncRunner(nil, APISyncOptions{
				ContractInternalURL: server.URL,
				ContractCredential:  MachineCredential{ClientID: "contract-reader"},
				TenantID:            "tenant-1",
			})
			runner.tokens["contract-reader"] = cachedMachineToken{
				value: "machine-token", expiresAt: time.Now().Add(time.Hour),
			}
			var captured contractDashboardSnapshot
			err := runner.syncContractDashboard(context.Background(), func(_ context.Context, snapshot contractDashboardSnapshot) error {
				captured = snapshot
				return nil
			})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) || captured.TenantID != "" {
					t.Fatalf("expected %q without publishing; snapshot=%+v err=%v", test.wantErr, captured, err)
				}
			} else if err != nil || captured.TenantID != "tenant-1" || captured.TotalContracts != 205 {
				t.Fatalf("complete aggregate not published: snapshot=%+v err=%v", captured, err)
			}
		})
	}
}

func TestSyncContractDashboardRejectsCrossTenantResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(response, `{"code":"OK","data":{"tenant_id":"tenant-2","total_contracts":9}}`)
	}))
	defer server.Close()

	runner := &APISyncRunner{
		options: APISyncOptions{
			ContractInternalURL: server.URL,
			ContractCredential:  MachineCredential{ClientID: "contract-reader"},
			TenantID:            "tenant-1",
			HTTPTimeout:         time.Second,
		},
		tokens: map[string]cachedMachineToken{"contract-reader": {value: "machine-token", expiresAt: time.Now().Add(time.Hour)}},
	}
	persisted := false
	err := runner.syncContractDashboard(context.Background(), func(context.Context, contractDashboardSnapshot) error {
		persisted = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "tenant mismatch") {
		t.Fatalf("syncContractDashboard() error = %v, want tenant mismatch", err)
	}
	if persisted {
		t.Fatal("cross-tenant contract snapshot was persisted")
	}
}

func TestSyncContractDashboardRequiresTenantBeforeCallingUpstream(t *testing.T) {
	t.Parallel()

	runner := &APISyncRunner{options: APISyncOptions{ContractInternalURL: "http://127.0.0.1:1"}}
	err := runner.syncContractDashboard(context.Background(), func(context.Context, contractDashboardSnapshot) error {
		t.Fatal("snapshot sink must not run without a configured tenant")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "tenant ID is required") {
		t.Fatalf("syncContractDashboard() error = %v, want required tenant error", err)
	}
}
