package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/comments"
	"Coves/internal/core/embeds"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"

	"github.com/rivo/uniseg"
)

// ProfileLookup batch-loads indexed profiles.
type ProfileLookup interface {
	GetByDIDs(ctx context.Context, dids []string) (map[string]*users.User, error)
}

// PostViewLookup batch-loads post views.
type PostViewLookup interface {
	GetViewsByURIs(ctx context.Context, uris []string, viewerDID string) (map[string]*posts.PostView, error)
}

// CommentLookup batch-loads comments.
type CommentLookup interface {
	GetByURIsBatch(ctx context.Context, uris []string) (map[string]*comments.Comment, error)
}

// ListService serves listNotifications.
type ListService interface {
	ListNotifications(ctx context.Context, recipientDID, cursor string, limit int) (ListNotificationsOutput, error)
}

type listService struct {
	repo          ReadRepository
	profiles      ProfileLookup
	postViews     PostViewLookup
	commentLookup CommentLookup
}

// NewListService builds the listNotifications service.
func NewListService(repo ReadRepository, profiles ProfileLookup, postViews PostViewLookup, commentLookup CommentLookup) ListService {
	return listService{repo: repo, profiles: profiles, postViews: postViews, commentLookup: commentLookup}
}

func (s listService) ListNotifications(ctx context.Context, recipientDID, cursor string, limit int) (ListNotificationsOutput, error) {
	page, err := s.repo.List(ctx, recipientDID, cursor, limit)
	if err != nil {
		return ListNotificationsOutput{}, fmt.Errorf("list notifications: %w", err)
	}
	output := ListNotificationsOutput{Notifications: make([]NotificationView, 0, len(page.Notifications)), Cursor: page.Cursor}
	if page.SeenAt != nil {
		output.SeenAt = notificationTime(*page.SeenAt)
	}

	var actorDIDs, postURIs, commentURIs []string
	actors, postsSeen, commentsSeen := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	add := func(key string, seen map[string]bool, keys *[]string) {
		if key != "" && !seen[key] {
			seen[key] = true
			*keys = append(*keys, key)
		}
	}
	for _, row := range page.Notifications {
		if row.Reason == ReasonUpvote {
			for _, voterDID := range row.RecentUpvoterDIDs {
				add(voterDID, actors, &actorDIDs)
			}
		} else {
			add(row.ActorDID, actors, &actorDIDs)
		}
		if row.RootPost.State == ReferenceLive {
			add(row.RootPostURI, postsSeen, &postURIs)
		}
		switch row.Reason {
		case ReasonUpvote:
			if row.Subject.State == ReferenceLive {
				if posts.IsPostCollection(posts.CollectionOfPostURI(row.SubjectURI)) {
					add(row.SubjectURI, postsSeen, &postURIs)
				} else {
					add(row.SubjectURI, commentsSeen, &commentURIs)
				}
			}
		case ReasonPostReply:
			if row.Record.State == ReferenceLive {
				add(row.RecordURI, commentsSeen, &commentURIs)
			}
			if row.Subject.State == ReferenceLive {
				add(row.SubjectURI, postsSeen, &postURIs)
			}
		case ReasonCommentReply:
			if row.Record.State == ReferenceLive {
				add(row.RecordURI, commentsSeen, &commentURIs)
			}
			if row.Subject.State == ReferenceLive {
				add(row.SubjectURI, commentsSeen, &commentURIs)
			}
		case ReasonMention:
			if row.Record.State == ReferenceLive {
				if posts.IsPostCollection(posts.CollectionOfPostURI(row.RecordURI)) {
					add(row.RecordURI, postsSeen, &postURIs)
				} else {
					add(row.RecordURI, commentsSeen, &commentURIs)
				}
			}
		}
	}

	var profiles map[string]*users.User
	if len(actorDIDs) > 0 {
		profiles, err = s.profiles.GetByDIDs(ctx, actorDIDs)
		if err != nil {
			return ListNotificationsOutput{}, fmt.Errorf("load notification authors: %w", err)
		}
	}
	var postViews map[string]*posts.PostView
	if len(postURIs) > 0 {
		postViews, err = s.postViews.GetViewsByURIs(ctx, postURIs, "")
		if err != nil {
			return ListNotificationsOutput{}, fmt.Errorf("load notification posts: %w", err)
		}
	}
	thumbnails := make(map[string]struct{ url, alt string }, len(postViews))
	for uri, post := range postViews {
		url, alt := posts.PreviewThumbnail(post)
		thumbnails[uri] = struct{ url, alt string }{url, alt}
	}
	var commentViews map[string]*comments.Comment
	if len(commentURIs) > 0 {
		commentViews, err = s.commentLookup.GetByURIsBatch(ctx, commentURIs)
		if err != nil {
			return ListNotificationsOutput{}, fmt.Errorf("load notification comments: %w", err)
		}
	}

	var omissions omittedRows
	for _, row := range page.Notifications {
		record, root := commentViews[row.RecordURI], postViews[row.RootPostURI]
		postMention := row.Reason == ReasonMention && posts.IsPostCollection(posts.CollectionOfPostURI(row.RecordURI))
		switch {
		case row.Reason != ReasonUpvote && row.Record.State == ReferenceLive && postMention && postViews[row.RecordURI] == nil:
			omissions.add(row.Reason, &omissions.missingRecord)
			continue
		case row.Reason != ReasonUpvote && row.Record.State == ReferenceLive && !postMention && record == nil:
			omissions.add(row.Reason, &omissions.missingRecord)
			continue
		case row.Reason != ReasonUpvote && row.Record.State == ReferenceLive && !postMention && record.DeletedAt != nil:
			omissions.add(row.Reason, &omissions.deletedRecord)
			continue
		case row.RootPost.State == ReferenceLive && root == nil:
			omissions.add(row.Reason, &omissions.missingRoot)
			continue
		}
		var recordLabels *posts.SelfLabels
		if row.Reason != ReasonUpvote && row.Record.State == ReferenceLive && !postMention {
			if recordLabels, err = notificationCommentLabels(record); err != nil {
				omissions.add(row.Reason, &omissions.malformedLabels)
				continue
			}
		}
		var subject *SubjectView
		switch row.Reason {
		case ReasonUpvote:
			subject = &SubjectView{URI: row.SubjectURI, CID: row.Subject.CID, Status: referenceStatus(row.Subject.State)}
			if row.Subject.State == ReferenceLive {
				if posts.IsPostCollection(posts.CollectionOfPostURI(row.SubjectURI)) {
					post := postViews[row.SubjectURI]
					if post == nil {
						omissions.add(row.Reason, &omissions.missingSubject)
						continue
					}
					subject.Preview = notificationPostPreview(post)
					subject.Labels = notificationPostLabels(post)
					subject.Thumbnail, subject.ThumbnailAlt = thumbnails[row.SubjectURI].url, thumbnails[row.SubjectURI].alt
				} else {
					comment := commentViews[row.SubjectURI]
					if comment == nil {
						omissions.add(row.Reason, &omissions.missingSubject)
						continue
					}
					if comment.DeletedAt != nil {
						omissions.add(row.Reason, &omissions.deletedSubject)
						continue
					}
					if subject.Labels, err = notificationCommentLabels(comment); err != nil {
						omissions.add(row.Reason, &omissions.malformedLabels)
						continue
					}
					subject.Preview = notificationExcerpt(comment.Content)
				}
			}
		case ReasonPostReply:
			subject = &SubjectView{URI: row.SubjectURI, CID: row.Subject.CID, Status: referenceStatus(row.Subject.State)}
			if row.Subject.State == ReferenceLive {
				post := postViews[row.SubjectURI]
				if post == nil {
					omissions.add(row.Reason, &omissions.missingSubject)
					continue
				}
				subject.Preview = notificationPostPreview(post)
				subject.Labels = notificationPostLabels(post)
				subject.Thumbnail, subject.ThumbnailAlt = thumbnails[row.SubjectURI].url, thumbnails[row.SubjectURI].alt
			}
		case ReasonCommentReply:
			subject = &SubjectView{URI: row.SubjectURI, CID: row.Subject.CID, Status: referenceStatus(row.Subject.State)}
			if row.Subject.State == ReferenceLive {
				comment := commentViews[row.SubjectURI]
				if comment == nil {
					omissions.add(row.Reason, &omissions.missingSubject)
					continue
				}
				if comment.DeletedAt != nil {
					omissions.add(row.Reason, &omissions.deletedSubject)
					continue
				}
				if subject.Labels, err = notificationCommentLabels(comment); err != nil {
					omissions.add(row.Reason, &omissions.malformedLabels)
					continue
				}
				subject.Preview = notificationExcerpt(comment.Content)
			}
		case ReasonMention:
			// Mentions have no subject reference.
		default:
			omissions.add(row.Reason, &omissions.unrecognizedReason)
			continue
		}

		rootView := &RootPostView{URI: row.RootPostURI, CID: row.RootPost.CID, Status: referenceStatus(row.RootPost.State)}
		if row.RootPost.State == ReferenceLive {
			if title := postRecordField(root, "title"); strings.TrimSpace(title) != "" {
				rootView.Title = title
			}
			rootView.Community = root.Community
			rootView.Labels = notificationPostLabels(root)
			rootView.Thumbnail, rootView.ThumbnailAlt = thumbnails[row.RootPostURI].url, thumbnails[row.RootPostURI].alt
		}
		view := NotificationView{Reason: row.Reason, SortAt: notificationTime(row.SortAt), IsRead: row.IsRead,
			RootPost: rootView, Subject: subject}
		if row.Reason == ReasonUpvote {
			view.UpvoteCount = row.UpvoteCount
			for _, voterDID := range row.RecentUpvoterDIDs {
				view.RecentUpvoters = append(view.RecentUpvoters, notificationProfileView(voterDID, profiles))
			}
		} else {
			author := notificationProfileView(row.ActorDID, profiles)
			view.Author = &author
			recordView := &RecordView{URI: row.RecordURI, CID: row.Record.CID, Status: referenceStatus(row.Record.State), CreatedAt: notificationTime(row.RecordCreatedAt)}
			if row.Record.State == ReferenceLive {
				if postMention {
					post := postViews[row.RecordURI]
					text, _ := notificationPostText(post)
					recordView.Excerpt = notificationExcerpt(text)
					recordView.Labels = notificationPostLabels(post)
					recordView.Thumbnail, recordView.ThumbnailAlt = thumbnails[row.RecordURI].url, thumbnails[row.RecordURI].alt
				} else {
					recordView.Excerpt = notificationExcerpt(record.Content)
					recordView.Labels = recordLabels
				}
			}
			view.Record = recordView
		}
		output.Notifications = append(output.Notifications, view)
	}
	omissions.log(ctx)
	return output, nil
}

func notificationProfileView(did string, profiles map[string]*users.User) ProfileView {
	view := ProfileView{DID: did}
	if profile := profiles[did]; profile != nil {
		view.Handle = profile.Handle
		if profile.DisplayName != "" {
			view.DisplayName = &profile.DisplayName
		}
		if avatarURL := blobs.HydrateImageURL(blobs.GetImageURLConfig(), profile.PDSURL, profile.DID, profile.AvatarCID, "avatar_small"); avatarURL != "" {
			view.Avatar = &avatarURL
		}
	}
	return view
}

func referenceStatus(state ReferenceState) string {
	switch state {
	case ReferenceDeleted:
		return "deleted"
	case ReferenceRemovedByModerator:
		return "removedByModerator"
	case ReferenceRemovedByServerAdmin:
		return "removedByServerAdmin"
	default:
		return ""
	}
}

// omittedRows counts listed rows dropped during hydration. It holds counts
// only, so its log line never carries URIs, DIDs or cursors.
type omittedRows struct {
	total, postReply, commentReply, otherReason                                                   int
	missingRecord, deletedRecord, missingRoot, missingSubject, deletedSubject, unrecognizedReason int
	malformedLabels                                                                               int
}

func (o *omittedRows) add(reason Reason, cause *int) {
	o.total++
	*cause++
	switch reason {
	case ReasonPostReply:
		o.postReply++
	case ReasonCommentReply:
		o.commentReply++
	default:
		o.otherReason++
	}
}

func (o omittedRows) log(ctx context.Context) {
	if o.total == 0 {
		return
	}
	slog.WarnContext(ctx, "notifications: omitted unhydratable rows",
		slog.Int("omitted", o.total),
		slog.Group("by_reason",
			slog.Int(string(ReasonPostReply), o.postReply),
			slog.Int(string(ReasonCommentReply), o.commentReply),
			slog.Int("other", o.otherReason)),
		slog.Group("by_cause",
			slog.Int("missing_record", o.missingRecord),
			slog.Int("deleted_record", o.deletedRecord),
			slog.Int("missing_root", o.missingRoot),
			slog.Int("missing_subject", o.missingSubject),
			slog.Int("deleted_subject", o.deletedSubject),
			slog.Int("unrecognized_reason", o.unrecognizedReason),
			slog.Int("malformed_labels", o.malformedLabels)))
}

func notificationTime(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

func postRecordField(post *posts.PostView, field string) string {
	record, ok := post.Record.(map[string]interface{})
	if !ok {
		return ""
	}
	value, _ := record[field].(string)
	return value
}

// notificationPostText chooses the same source for subjects and post mentions.
// Callers apply their own excerpt rule to the selected text.
func notificationPostText(post *posts.PostView) (text string, isTitle bool) {
	if title := postRecordField(post, "title"); strings.TrimSpace(title) != "" {
		return title, true
	}
	if embed, ok := post.Embed.(map[string]interface{}); ok {
		if kind := embed["$type"]; kind == embeds.TypeImages || kind == embeds.TypeImages+"#view" {
			return "", false
		}
	}
	return postRecordField(post, "content"), false
}

func notificationPostPreview(post *posts.PostView) string {
	text, isTitle := notificationPostText(post)
	if isTitle {
		return text
	}
	return notificationExcerpt(text)
}

func notificationPostLabels(post *posts.PostView) *posts.SelfLabels {
	record, ok := post.Record.(map[string]interface{})
	if !ok {
		return nil
	}
	switch labels := record["labels"].(type) {
	case posts.SelfLabels:
		return boundedNotificationLabels(&labels)
	case *posts.SelfLabels:
		return boundedNotificationLabels(labels)
	default:
		return nil
	}
}

// notificationCommentLabels returns an error when the stored self-labels do
// not decode; the caller omits the row rather than show it unlabelled.
func notificationCommentLabels(comment *comments.Comment) (*posts.SelfLabels, error) {
	if comment.ContentLabels == nil {
		return nil, nil
	}
	var labels posts.SelfLabels
	if err := json.Unmarshal([]byte(*comment.ContentLabels), &labels); err != nil {
		return nil, fmt.Errorf("decode comment self-labels: %w", err)
	}
	return boundedNotificationLabels(&labels), nil
}

func boundedNotificationLabels(labels *posts.SelfLabels) *posts.SelfLabels {
	if labels == nil || len(labels.Values) == 0 {
		return nil
	}
	bounded := &posts.SelfLabels{Values: make([]posts.SelfLabel, 0, min(len(labels.Values), 10))}
	for _, label := range labels.Values {
		if len(label.Val) <= 128 {
			bounded.Values = append(bounded.Values, label)
			if len(bounded.Values) == 10 {
				break
			}
		}
	}
	if len(bounded.Values) == 0 {
		return nil
	}
	return bounded
}

func notificationExcerpt(text string) string {
	clusters := uniseg.NewGraphemes(text)
	for count := 0; count < 140; count++ {
		if !clusters.Next() {
			return text
		}
	}
	_, end := clusters.Positions()
	return text[:end]
}
