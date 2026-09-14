package discover

import "context"

// DiscoverHotStateCleaner removes expired derived ranking state in bounded passes.
type DiscoverHotStateCleaner interface {
	CleanupExpiredDiscoverHotState(ctx context.Context, derivedRowBatchSize int) (int64, error)
}
