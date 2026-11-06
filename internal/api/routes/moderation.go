package routes

import (
	handler "Coves/internal/api/handlers/moderation"
	"Coves/internal/api/middleware"
	"Coves/internal/core/accountbans"
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
	getViewerAuthority := handler.NewGetViewerAuthorityHandler(authority, handlerOptions...)
	r.With(adminAuth.RequireAuthenticated).Get(
		"/xrpc/social.coves.actor.getViewerAuthority", getViewerAuthority.HandleGetViewerAuthority)
}

// ViewerAuthorityRouteOption configures RegisterViewerAuthorityRoute.
type ViewerAuthorityRouteOption func(*viewerAuthorityRouteOptions)

type viewerAuthorityRouteOptions struct {
	accountBans accountbans.Reader
}

// WithViewerAuthorityAccountBans installs the account-ban read that adds the
// viewer's ban notice to getViewerAuthority.
func WithViewerAuthorityAccountBans(reader accountbans.Reader) ViewerAuthorityRouteOption {
	return func(options *viewerAuthorityRouteOptions) { options.accountBans = reader }
}
