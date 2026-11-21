package routes

import (
	handler "Coves/internal/api/handlers/moderation"
	"Coves/internal/api/middleware"
	"Coves/internal/core/accountbans"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/go-chi/chi/v5"
)

// RegisterModerationRoutes registers the social.coves.moderation.* endpoints.
// Moderation NSIDs are AppView-served, never PDS-proxied, so the PDS OAuth
// scopes in cmd/server remain unchanged (see oauth_scopes_test.go).
func RegisterModerationRoutes(r chi.Router, service moderation.Service, adminAuth *middleware.InstanceAdminMiddleware) {
	getSubjectState := handler.NewGetSubjectStateHandler(service)
	listActions := handler.NewListActionsHandler(service)
	listAdminActions := handler.NewListAdminActionsHandler(service)
	removeContent := handler.NewRemoveContentHandler(service)
	restoreContent := handler.NewRestoreContentHandler(service)
	labelContent := handler.NewLabelContentHandler(service)
	retractContentLabel := handler.NewRetractContentLabelHandler(service)
	banAccount := handler.NewBanAccountHandler(service)
	unbanAccount := handler.NewUnbanAccountHandler(service)
	removeCommunity := handler.NewRemoveCommunityHandler(service)
	restoreCommunity := handler.NewRestoreCommunityHandler(service)
	r.Get("/xrpc/social.coves.moderation.listActions", listActions.HandleListActions)
	r.With(adminAuth.RequireInstanceAdmin).Get(
		"/xrpc/social.coves.moderation.listAdminActions", listAdminActions.HandleListAdminActions)
	r.With(adminAuth.RequireInstanceAdmin).Get(
		"/xrpc/social.coves.moderation.getSubjectState", getSubjectState.HandleGetSubjectState)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.removeContent", removeContent.HandleRemoveContent)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.restoreContent", restoreContent.HandleRestoreContent)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.labelContent", labelContent.HandleLabelContent)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.retractContentLabel", retractContentLabel.HandleRetractContentLabel)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.banAccount", banAccount.HandleBanAccount)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.unbanAccount", unbanAccount.HandleUnbanAccount)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.removeCommunity", removeCommunity.HandleRemoveCommunity)
	r.With(adminAuth.RequireInstanceAdmin).Post(
		"/xrpc/social.coves.moderation.restoreCommunity", restoreCommunity.HandleRestoreCommunity)
}

// RegisterViewerAuthorityRoute registers social.coves.actor.getViewerAuthority.
// It is AppView-served and authenticated, never PDS-proxied, so it stays out
// of the PDS OAuth scopes.
func RegisterViewerAuthorityRoute(r chi.Router, adminAuth *middleware.InstanceAdminMiddleware, authority moderation.Authority, opts ...ViewerAuthorityRouteOption) {
	var options viewerAuthorityRouteOptions
	for _, opt := range opts {
		opt(&options)
	}
	var handlerOptions []handler.GetViewerAuthorityOption
	if options.accountBans != nil {
		handlerOptions = append(handlerOptions, handler.WithAccountBanNotice(options.accountBans))
	}
	if options.communityRoles != nil {
		handlerOptions = append(handlerOptions, handler.WithCommunityRoles(options.communityRoles))
	}
	getViewerAuthority := handler.NewGetViewerAuthorityHandler(authority, handlerOptions...)
	r.With(adminAuth.RequireAuthenticated).Get(
		"/xrpc/social.coves.actor.getViewerAuthority", getViewerAuthority.HandleGetViewerAuthority)
}

// ViewerAuthorityRouteOption configures RegisterViewerAuthorityRoute.
type ViewerAuthorityRouteOption func(*viewerAuthorityRouteOptions)

type viewerAuthorityRouteOptions struct {
	accountBans    accountbans.Reader
	communityRoles communities.RoleAuthorizer
}

// WithViewerAuthorityAccountBans installs the account-ban read that adds the
// viewer's ban notice to getViewerAuthority.
func WithViewerAuthorityAccountBans(reader accountbans.Reader) ViewerAuthorityRouteOption {
	return func(options *viewerAuthorityRouteOptions) { options.accountBans = reader }
}

// WithViewerAuthorityCommunityRoles installs the community role authorizer
// that adds whether the viewer holds a usable community role to
// getViewerAuthority.
func WithViewerAuthorityCommunityRoles(authorizer communities.RoleAuthorizer) ViewerAuthorityRouteOption {
	return func(options *viewerAuthorityRouteOptions) { options.communityRoles = authorizer }
}

// RegisterCommunityStateRoute registers social.coves.moderation.getCommunityState.
// It is AppView-served and authenticated; the service authorizes the viewer
// as a moderator of the subject's community or an instance admin.
func RegisterCommunityStateRoute(r chi.Router, adminAuth *middleware.InstanceAdminMiddleware, service handler.CommunityStateGetter) {
	getCommunityState := handler.NewGetCommunityStateHandler(service)
	r.With(adminAuth.RequireAuthenticated).Get(
		"/xrpc/social.coves.moderation.getCommunityState", getCommunityState.HandleGetCommunityState)
}

// RegisterCommunityRosterRoutes registers social.coves.community.resignModerator
// and social.coves.community.removeModerator.
func RegisterCommunityRosterRoutes(r chi.Router, adminAuth *middleware.InstanceAdminMiddleware, service handler.CommunityRosterActions) {
	roster := handler.NewCommunityRosterHandler(service)
	r.With(adminAuth.RequireAuthenticated).Post("/xrpc/social.coves.community.resignModerator", roster.HandleResign)
	r.With(adminAuth.RequireAuthenticated).Post("/xrpc/social.coves.community.removeModerator", roster.HandleRemove)
}

// RegisterCommunityInvitationRoutes registers
// social.coves.community.inviteModerator, offerOwnership, acceptInvitation,
// declineInvitation, listInvitations and listViewerInvitations.
func RegisterCommunityInvitationRoutes(r chi.Router, adminAuth *middleware.InstanceAdminMiddleware, service handler.CommunityInvitationActions) {
	invitations := handler.NewCommunityInvitationHandler(service)
	authenticated := r.With(adminAuth.RequireAuthenticated)
	authenticated.Post("/xrpc/social.coves.community.inviteModerator", invitations.HandleInviteModerator)
	authenticated.Post("/xrpc/social.coves.community.offerOwnership", invitations.HandleOfferOwnership)
	authenticated.Post("/xrpc/social.coves.community.acceptInvitation", invitations.HandleAcceptInvitation)
	authenticated.Post("/xrpc/social.coves.community.declineInvitation", invitations.HandleDeclineInvitation)
	authenticated.Get("/xrpc/social.coves.community.listInvitations", invitations.HandleListInvitations)
	authenticated.Get("/xrpc/social.coves.community.listViewerInvitations", invitations.HandleListViewerInvitations)
}

// RegisterCommunityPostRoutes registers social.coves.community.removePost and
// social.coves.community.restorePost.
func RegisterCommunityPostRoutes(r chi.Router, adminAuth *middleware.InstanceAdminMiddleware, service handler.CommunityPostActions) {
	postActions := handler.NewCommunityPostHandler(service)
	authenticated := r.With(adminAuth.RequireAuthenticated)
	authenticated.Post("/xrpc/social.coves.community.removePost", postActions.HandleRemovePost)
	authenticated.Post("/xrpc/social.coves.community.restorePost", postActions.HandleRestorePost)
}

// RegisterCommunityCommentRoutes registers social.coves.community.removeComment
// and social.coves.community.restoreComment.
func RegisterCommunityCommentRoutes(r chi.Router, adminAuth *middleware.InstanceAdminMiddleware, service handler.CommunityCommentActions) {
	commentActions := handler.NewCommunityCommentHandler(service)
	authenticated := r.With(adminAuth.RequireAuthenticated)
	authenticated.Post("/xrpc/social.coves.community.removeComment", commentActions.HandleRemoveComment)
	authenticated.Post("/xrpc/social.coves.community.restoreComment", commentActions.HandleRestoreComment)
}

// RegisterCommunityBanRoutes registers social.coves.community.banAccount,
// unbanAccount and listBans.
func RegisterCommunityBanRoutes(r chi.Router, adminAuth *middleware.InstanceAdminMiddleware, service handler.CommunityBanActions) {
	bans := handler.NewCommunityBanHandler(service)
	authenticated := r.With(adminAuth.RequireAuthenticated)
	authenticated.Post("/xrpc/social.coves.community.banAccount", bans.HandleBanAccount)
	authenticated.Post("/xrpc/social.coves.community.unbanAccount", bans.HandleUnbanAccount)
	authenticated.Get("/xrpc/social.coves.community.listBans", bans.HandleListBans)
}
