// dashboard-api：嵌入桥 + 业务 API。
// OIDC 会话 / embed / 预警中心 / 字典 / 管理（对齐 CRM crm-server 装配模式）。
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/bootstrap"
	"github.com/unified-identity-auth-platform/data-analysis/internal/platformcatalog"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shutdown"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// B2：关闭预算在启动期解析，非法值失败关闭，避免配置错误静默退回默认值。
	drainBudget, err := shutdown.BudgetFromEnv(os.Getenv)
	if err != nil {
		logger.Error("data-analysis shutdown budget invalid", "error", err)
		os.Exit(1)
	}
	config, err := bootstrap.LoadConfig()
	if err != nil {
		logger.Error("data-analysis configuration failed", "error", err)
		os.Exit(1)
	}
	manifest := platformcatalog.DataAnalysisManifest()
	if config.OIDCMaxRoles != manifest.Policy.MaxEffectiveRoles {
		logger.Error("data-analysis OIDC role policy is incompatible", "expected_max_roles", manifest.Policy.MaxEffectiveRoles)
		os.Exit(1)
	}
	// 运行时 OIDC 目录哈希已配置时强校验与内置目录一致；未配置（本地骨架/首次接入前）跳过，
	// 避免接入尚未完成时阻塞启动。
	if config.OIDCRoleConfigHash != "" {
		if err := platformcatalog.ValidateClaimsRoleConfigHash(manifest, config.OIDCRoleConfigHash); err != nil {
			logger.Error("data-analysis authorization catalog is incompatible", "error", err)
			os.Exit(1)
		}
	}
	if err := platformcatalog.Publish(context.Background(), manifest, platformcatalog.Options{
		Enabled: config.CatalogSyncEnabled, BaseURL: config.PlatformBaseURL, ApplicationID: config.CatalogApplicationID,
		ClientID: config.CatalogClientID, ClientSecret: config.CatalogClientSecret,
	}); err != nil {
		logger.Error("data-analysis authorization catalog synchronization failed", "error", err)
		os.Exit(1)
	}
	app, err := bootstrap.New(config)
	if err != nil {
		logger.Error("data-analysis startup failed", "error", err)
		os.Exit(1)
	}
	defer app.StopLicense()
	// HTTP 服务与 bootstrap 里的后台循环（事务内审计 outbox 投递）同属一个 join 集合：
	// 收到 SIGTERM 后先 Shutdown 停止接新请求，再在统一预算内等两者退出。
	serverResult := app.Background.Run("http-server", func() error {
		logger.Info("dashboard-api listening", "addr", config.ListenAddr)
		if err := app.Server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
	case serveErr := <-serverResult:
		// 服务自行停止（外部关闭或监听失败）：监听失败仍按非零退出，其余走统一收尾。
		if serveErr != nil {
			logger.Error("dashboard-api server failed", "error", serveErr)
			os.Exit(1)
		}
	}

	// 单次预算同时约束 HTTP 排空与后台 join，避免两段串行把总时长放大到 2×budget。
	deadline := time.Now().Add(drainBudget)
	ctx, cancel := context.WithTimeout(context.Background(), drainBudget)
	if err := app.Server.Shutdown(ctx); err != nil {
		// 排空失败不改变退出码判断口径：是否“有组件没退出”由下面的 join 点名。
		logger.Error("dashboard-api http shutdown incomplete", "error", err)
	}
	cancel()
	if app.StopBackground != nil {
		app.StopBackground()
	}
	if err := app.Background.Join(time.Until(deadline)); err != nil {
		// 超预算必须点名未退出的组件并非零退出：不能把“投递循环还在跑”伪装成正常关闭。
		logger.Error("dashboard-api shutdown incomplete", "error", err)
		os.Exit(1)
	}
	logger.Info("dashboard-api stopped")
}
