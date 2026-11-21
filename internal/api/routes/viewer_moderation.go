package routes

import "Coves/internal/core/moderation"

// ViewerModerationRouteOption configures the viewer moderation lists the
// feed, timeline, discover and actor reads serve.
type ViewerModerationRouteOption func(*viewerModerationRouteConfig)

type viewerModerationRouteConfig struct {
	communityViewer moderation.CommunityViewer
}

// WithCommunityViewerModeration supplies the community viewer reader, so the
// reads serve viewer.communityModeration. Without it they serve none.
func WithCommunityViewerModeration(viewer moderation.CommunityViewer) ViewerModerationRouteOption {
	return func(c *viewerModerationRouteConfig) { c.communityViewer = viewer }
}

func newViewerModerationRouteConfig(opts []ViewerModerationRouteOption) viewerModerationRouteConfig {
	var cfg viewerModerationRouteConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
