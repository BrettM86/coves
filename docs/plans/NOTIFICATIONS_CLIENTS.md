# Notification clients

Plan, not built. The backend (`social.coves.notification.*`) is on main; its contract, product decisions and gates are in [NOTIFICATIONS.md](../NOTIFICATIONS.md). This file covers the web and mobile clients and the rollout. Delete it when both clients ship.

## Client contract

- In-app only. No push; clients poll.
- `reason` is an open set (`postReply`, `commentReply`, `mention`, `upvote`), and so is `status`. Clients must tolerate values they do not know.
- Unread is `sortAt > seenAt`, where `sortAt` is the AppView's index time. An account that has never marked anything seen has no `seenAt`, and only its newest row is unread. After page one renders, send the largest `sortAt` displayed to `updateSeen`, echoed verbatim and never the client clock; the server clamps it to server time and never moves it backwards. Rows carry `isRead`, but compute read state for later pages against page one's `seenAt`, so the `updateSeen` after page one does not mark deeper pages read. Set the badge from the next `getUnreadCount`, not by zeroing it optimistically.
- `getUnreadCount` returns 0 to 101; 101 means "100+".
- Keep paging while `cursor` is present. A page can hold fewer rows than `limit`, even zero: rows whose references are not yet indexed or were deleted mid-request are omitted, never half-filled.
- Upvote groups move up when bumped, so a bumped group can be skipped by later pages. Reload page one on pull-to-refresh and whenever the unread count rises.
- A reference (`rootPost`, `subject`, `record`) with `status` is a placeholder: render "[Deleted Comment]", "[Deleted Post]" or "[Removed by moderator]" in place of its text. Known values are `deleted`, `removedByModerator` (the community removed it) and `removedByServerAdmin` (an instance admin removed it); the label for the last is not decided yet. It carries no title, community, preview, labels or thumbnail. Placeholder rows open the thread like any other row; the thread shows its own deleted or removed state.
- Each reference carries its own author's self-labels in `labels`. Blur by the viewer's setting, as feeds do. A reply under an NSFW post carries the label on `rootPost` only, and the whole notification should blur.
- A post preview may carry `thumbnail` and, for an image post, `thumbnailAlt`. An image post shows its thumbnail and no body text; an untitled image post whose image cannot be shown has no text. The server already truncates excerpts to 140 grapheme clusters.
- `author` and `recentUpvoters` entries for accounts the AppView has not indexed carry only a `did`, with no handle.
- `upvoteCount` includes bridged (Lemmy) upvotes. `recentUpvoters` lists up to 3 native voters and is omitted when every counted upvote is bridged; render "N people upvoted your post" without names.
- Errors: `InvalidCursor` (400) on `listNotifications`; `AccountNotIndexed` (400) on `updateSeen` and `putPreferences`.
- Preferences are AppView-local, default to all on, and `putPreferences` takes a partial update and returns the full object.

## Frontend (coves-frontend)

- Route `/notifications`, auth required, with infinite scroll. Rows render per reason with an unread highlight. Replies and comment mentions open `rootPost` with the comment focused; post mentions and upvote groups open the subject or `rootPost`. A target that no longer exists shows the existing not-found state.
- Nav bell with a badge. Poll `getUnreadCount` every 30 seconds while `document.visibilityState` is visible, and once on focus; stop when logged out. Show "99+" above 99. When the count rises while `/notifications` is open, reload page one.
- Settings: a "Notifications" section in `/settings` with four toggles (one per reason) built on `ToggleSetting.svelte`, backed by `getPreferences` and `putPreferences`.
- API: NSIDs in `src/lib/api/coves/client.ts`, types in `types.ts`.
- i18n: replace the Photon `inbox` keys in `en.json`. Other locales fall back to English.
- States: empty, retryable error, and 401 through the existing session-expiration flow.

## Mobile (coves-mobile)

- Replace the placeholder `NotificationsScreen` (`lib/screens/home/notifications_screen.dart`) with a paginated list backed by a new `NotificationsProvider` in `lib/providers/` and `NotificationService` in `lib/services/`, using `coves_http.dart` and `auth_interceptor.dart`.
- Badge on the bell nav item in `main_shell_screen.dart` (`_buildNavItem(3, 'bell', …)`). Refresh on `AppLifecycleState.resumed`, on tab switch, and every 30 seconds while foregrounded.
- Mark seen by the same rule as web.
- Every row opens `PostDetailScreen` for `rootPost.uri`, passing `focusCommentUri` for replies, comment mentions and comment upvotes. Do not use `FocusedThreadScreen` directly: it needs a hydrated thread and provider, while `PostDetailScreen` already fetches and focuses a comment outside the loaded tree. A missing target shows the existing not-found state.
- Settings: four switches on the existing settings or profile screen.
- Pull-to-refresh replaces page one. `InvalidCursor` follows the Discover recovery rules: replace page one once, then offer a manual retry.

## Client tests

- Frontend: client and polling-store unit tests (visibility pause, logout stop, reload on count rise), plus browser tests for the route, badge, mark-seen watermark, page-one `seenAt` read state, and settings.
- Mobile: provider and service unit tests; widget tests for the list, badge, empty and error states, and navigation. One integration test against the local dev stack, using the local PLC and PDS with real accounts.

The backend pipeline for each reason is already covered at T2 with real sign-in (`AppView.SignIn`, see [TEST_ARCHITECTURE.md](../TEST_ARCHITECTURE.md)), so client tests do not need to re-prove fan-out.

## Chunks

One `/prd-loop` per repo, after the backend merges.

- coves-frontend: (1) API client and types, polling store, nav badge; (2) `/notifications` route with the mark-seen watermark and page-one read state; (3) settings toggles.
- coves-mobile: (1) service and provider, nav badge; (2) notifications screen, `PostDetailScreen` navigation, mark-seen; (3) settings toggles.

Chunk 2 in each repo is built manually, outside `/prd-loop`, because its visual design is decided while building it.

## Rollout

1. The backend ships first. Its endpoints are additive, and the migration sets the activation time; nothing before it notifies.
2. Frontend and mobile ship independently after that. Older mobile builds keep the placeholder tab.
3. No feature flag.
