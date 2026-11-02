package bootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/internal/importreport"
)

// writeReportStatusPublicKey 生成临时 Ed25519 公钥 PEM。
// 参数：t 为测试上下文。
// 返回值：公钥文件路径。
func writeReportStatusPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report-status.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReportStatusVerifierRequiresCompleteBinding 验证机器报表状态端点的装配门禁
// （BIZ-XAPI-13）：OAuth 客户端绑定缺失时拒绝构造验证器 → App.New 不挂载端点；
// 绑定齐全时构造成功；租户不一致时启动失败。
func TestReportStatusVerifierRequiresCompleteBinding(t *testing.T) {
	keyPath := writeReportStatusPublicKey(t)
	complete := importreport.VerifierOptions{
		Issuer: "platform", Audience: "analysis", PublicKeyPath: keyPath, ClientID: "contract-reader",
		OAuthClientID: "machine-client", TenantID: "tenant-1", CallerEnvironment: "test",
		RequireApplicationID: true, RequireEnvironmentID: true,
	}

	t.Run("endpoint disabled without any binding", func(t *testing.T) {
		verifier, err := reportStatusVerifier(Config{OIDCTenantID: "tenant-1"})
		if verifier != nil || err != nil {
			t.Fatalf("verifier=%v err=%v, want disabled endpoint", verifier, err)
		}
	})

	t.Run("missing oauth client disables endpoint", func(t *testing.T) {
		options := complete
		options.OAuthClientID = ""
		verifier, err := reportStatusVerifier(Config{ContractImportStatus: options, OIDCTenantID: "tenant-1"})
		if verifier != nil || err != nil {
			t.Fatalf("verifier=%v err=%v, want endpoint not mounted", verifier, err)
		}
	})

	t.Run("complete binding constructs verifier", func(t *testing.T) {
		verifier, err := reportStatusVerifier(Config{ContractImportStatus: complete, OIDCTenantID: "tenant-1"})
		if err != nil || verifier == nil {
			t.Fatalf("verifier=%v err=%v, want constructed verifier", verifier, err)
		}
	})

	t.Run("trusted tenant mismatch fails startup", func(t *testing.T) {
		verifier, err := reportStatusVerifier(Config{ContractImportStatus: complete, OIDCTenantID: "tenant-2"})
		if err == nil || verifier != nil {
			t.Fatalf("verifier=%v err=%v, want startup error", verifier, err)
		}
	})

	t.Run("unusable public key fails startup", func(t *testing.T) {
		options := complete
		options.PublicKeyPath = filepath.Join(t.TempDir(), "missing.pem")
		verifier, err := reportStatusVerifier(Config{ContractImportStatus: options, OIDCTenantID: "tenant-1"})
		if err == nil || verifier != nil {
			t.Fatalf("verifier=%v err=%v, want startup error", verifier, err)
		}
	})
}
