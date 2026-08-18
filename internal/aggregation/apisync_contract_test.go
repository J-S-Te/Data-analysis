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
			TenantID:            tenantID,
			HTTPTimeout:         time.Second,
		},
		token:          "machine-token",
		tokenExpiresAt: time.Now().Add(time.Hour),
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
		captured.ActiveContracts != 4 || captured.ExpiredContracts != 1 {
		t.Fatalf("captured snapshot = %#v", captured)
	}
	if captured.SnapshotAt.IsZero() || captured.SnapshotAt.Location() != time.UTC {
		t.Fatalf("snapshot time = %v, want a UTC timestamp", captured.SnapshotAt)
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
			TenantID:            "tenant-1",
			HTTPTimeout:         time.Second,
		},
		token:          "machine-token",
		tokenExpiresAt: time.Now().Add(time.Hour),
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
