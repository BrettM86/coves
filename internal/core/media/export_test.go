package media

import "context"

func HoldDecodeSlot(p *Pipeline, ctx context.Context) (release func(), err error) {
	return p.acquireDecodeSlot(ctx)
}

// SetBeforeOrientedEncode runs hook after a JPEG is oriented and before it is
// re-encoded, so a test can observe the decode slot during that work.
func SetBeforeOrientedEncode(p *Pipeline, hook func()) {
	p.beforeOrientedEncode = hook
}

// SetBeforeDownscaleEncode runs hook after an upload is downscaled and before
// it is re-encoded, so a test can observe the decode slot during that work.
func SetBeforeDownscaleEncode(p *Pipeline, hook func()) {
	p.beforeDownscaleEncode = hook
}

const MaxJPEGScans = maxJPEGScans

var NewUploadLimiterWithClock = newUploadLimiterWithClock
