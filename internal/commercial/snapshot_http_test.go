package commercial

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	core "github.com/J-S-Te/license-core"
	runtime "github.com/J-S-Te/license-core/runtime"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSignedOfflineSnapshotExpiryControlsRealHTTPPaths(t *testing.T) {
	// Keys are isolated test fixtures, not distribution trust or vendor secrets.
	pp, ps, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vp, vs, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	binding := runtime.Binding{InstanceID: "installation", Environment: "production", Application: "data_analysis", ServiceID: "dashboard-api"}
	path := filepath.Join(t.TempDir(), "state.json")
	keys := map[string]ed25519.PublicKey{"platform-test": pp}
	vendors := map[string]ed25519.PublicKey{"vendor-test": vp}
	rt, err := runtime.NewFileRuntime(path, binding, keys, vendors)
	if err != nil {
		t.Fatal(err)
	}
	license, err := core.Sign(core.License{ProtocolVersion: 1, Issuer: core.Issuer, ID: "test-license", Version: 1, CustomerID: "test-customer", ProductID: core.Product, Environment: binding.Environment, InstanceID: binding.InstanceID, IssuedAt: now.Unix() - 1000, NotBefore: now.Unix() - 100, Applications: []core.Application{{Code: "data_analysis", NotBefore: now.Unix() - 100, ExpiresAt: now.Unix() + 60, Kind: "FULL"}}}, "vendor-test", vs)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.SignSnapshot(runtime.PlatformSnapshot{Protocol: 1, InstanceID: binding.InstanceID, Environment: binding.Environment, Application: binding.Application, ServiceID: binding.ServiceID, Revision: 1, EnforcementState: runtime.Enforced, CurrentLicenseJWS: license, HighestLicenseVersion: 1, SnapshotIssuedAt: now.Unix()}, "platform-test", ps)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.ApplySnapshot(context.Background(), snapshot, now); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware(func(ctx context.Context, op string) error { return rt.Evaluate(ctx, core.Operation(op), now) }))
	for _, p := range []string{"/api/v1/dashboard/project", "/api/v1/embed/:dashboard", "/api/v1/embed-proxy/:token", "/api/v1/embed-proxy/:token/*resource"} {
		r.GET(p, func(c *gin.Context) { c.Status(200) })
	}
	request := func(p string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != want {
			t.Fatal(p, w.Code, want)
		}
	}
	request("/api/v1/embed/contract", 200)
	// No platform is connected. Offline elapsed time alone causes denial.
	now = now.Add(60 * time.Second)
	rt, err = runtime.NewFileRuntime(path, binding, keys, vendors)
	if err != nil {
		t.Fatal(err)
	}
	request("/api/v1/dashboard/project", 200)
	request("/api/v1/embed/contract", 403)
	request("/api/v1/embed-proxy/previously-issued-token", 403)
	request("/api/v1/embed-proxy/previously-issued-token/app.js", 403)
}
