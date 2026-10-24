package commercial

import (
	"context"
	core "github.com/J-S-Te/license-core"
	"github.com/J-S-Te/license-core/consumer"
	"log/slog"
)

func Start(ctx context.Context) (Check, error) {
	g, err := consumer.FromEnvironment("data_analysis")
	if err != nil {
		return nil, err
	}
	go func() {
		if err := g.Run(ctx, func(error) { slog.Warn("commercial license synchronization unavailable") }); err != nil && ctx.Err() == nil {
			slog.Error("commercial license synchronization stopped")
		}
	}()
	return func(ctx context.Context, operation string) error { return g.Check(ctx, core.Operation(operation)) }, nil
}
