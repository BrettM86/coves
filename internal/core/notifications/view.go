package notifications

import "Coves/internal/core/posts"

// ListNotificationsOutput is the social.coves.notification.listNotifications response.
type ListNotificationsOutput struct {
	Notifications []NotificationView `json:"notifications"`
	Cursor        string             `json:"cursor,omitempty"`
	SeenAt        string             `json:"seenAt,omitempty"`
}

// NotificationView is social.coves.notification.defs#notificationView.
type NotificationView struct {
	Reason         Reason        `json:"reason"`
	SortAt         string        `json:"sortAt"`
	IsRead         bool          `json:"isRead"`
	RootPost       *RootPostView `json:"rootPost,omitempty"`
	Subject        *SubjectView  `json:"subject,omitempty"`
	Record         *RecordView   `json:"record,omitempty"`
	Author         *ProfileView  `json:"author,omitempty"`
	UpvoteCount    int           `json:"upvoteCount,omitempty"`
	RecentUpvoters []ProfileView `json:"recentUpvoters,omitempty"`
}

// RootPostView is the thread's root post.
type RootPostView struct {
	URI          string              `json:"uri"`
	CID          string              `json:"cid"`
	Title        string              `json:"title,omitempty"`
	Community    *posts.CommunityRef `json:"community,omitempty"`
	Status       string              `json:"status,omitempty"`
	Labels       *posts.SelfLabels   `json:"labels,omitempty"`
	Thumbnail    string              `json:"thumbnail,omitempty"`
	ThumbnailAlt string              `json:"thumbnailAlt,omitempty"`
}

// SubjectView is the recipient's own post or comment.
type SubjectView struct {
	URI          string            `json:"uri"`
	CID          string            `json:"cid"`
	Preview      string            `json:"preview,omitempty"`
	Status       string            `json:"status,omitempty"`
	Labels       *posts.SelfLabels `json:"labels,omitempty"`
	Thumbnail    string            `json:"thumbnail,omitempty"`
	ThumbnailAlt string            `json:"thumbnailAlt,omitempty"`
}

// RecordView is the record that produced the notification.
type RecordView struct {
	URI          string            `json:"uri"`
	CID          string            `json:"cid"`
	Excerpt      string            `json:"excerpt,omitempty"`
	CreatedAt    string            `json:"createdAt"`
	Status       string            `json:"status,omitempty"`
	Labels       *posts.SelfLabels `json:"labels,omitempty"`
	Thumbnail    string            `json:"thumbnail,omitempty"`
	ThumbnailAlt string            `json:"thumbnailAlt,omitempty"`
}

// ProfileView is social.coves.actor.defs#profileView.
type ProfileView struct {
	DID         string  `json:"did"`
	Handle      string  `json:"handle,omitempty"`
	DisplayName *string `json:"displayName,omitempty"`
	Avatar      *string `json:"avatar,omitempty"`
}
