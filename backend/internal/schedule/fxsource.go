package schedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"finance-tracker/internal/fx"
)

// FxSourceAdapter adapts the shared fx.Client to report.FxSource.
type FxSourceAdapter struct {
	fx *fx.Client
}

func NewFxSourceAdapter(c *fx.Client) *FxSourceAdapter {
	return &FxSourceAdapter{fx: c}
}

// Factor implements report.FxSource. A persistent no-rate (fx.ErrNoRate)
// means "no conversion" per the collector contract: (0, false, nil), the
// row stays in its original currency. Other errors pass through.
func (a *FxSourceAdapter) Factor(ctx context.Context, date time.Time, from, to string) (float64, bool, error) {
	r, err := a.fx.Rate(ctx, date, from, to)
	if err != nil {
		if errors.Is(err, fx.ErrNoRate) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("schedule: fx factor %s->%s: %w", from, to, err)
	}
	return r.Factor, r.Converted, nil
}
