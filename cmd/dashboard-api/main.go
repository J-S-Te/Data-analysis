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
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

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
	go func() {
		logger.Info("dashboard-api listening", "addr", config.ListenAddr)
		if err := app.Server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("dashboard-api server failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = app.Server.Shutdown(ctx)
	logger.Info("dashboard-api stopped")
}
