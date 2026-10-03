package moderation_test

import (
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A restricted subject is projected like a hidden one: the public view names
// neither the subject nor a community scope, and the admin view keeps the
// subject in privateSubject.
func TestModlogRestrictedSubjectProjection(t *testing.T) {
	for _, reason := range []string{modlogReasonSpam, modlogReasonIllegal} {
		t.Run(reason, func(t *testing.T) {
			action := modlogTestAction()
			action.Reason = reason
			action.SubjectAccess = moderation.SubjectAccessRestricted
			action.ScopeKind = "community"
			action.ScopeCommunityDID = "did:plc:scopecommunity"
			public := moderation.NewActionView(action)
			object, raw := modlogJSON(t, public)
			assert.Nil(t, public.Subject)
			assert.NotContains(t, object, "subject")
			assert.Equal(t, moderation.ScopeView{Kind: "community"}, public.Scope)
			for _, private := range []string{action.SubjectURI, action.ObservedCID, action.ScopeCommunityDID, action.SubjectCommunityDID} {
				assert.NotContains(t, string(raw), private)
			}
			assert.Equal(t, action.Reason, public.Reason)

			admin := moderation.NewAdminActionView(action)
			assert.Equal(t, public, admin.Action)
			assert.Equal(t, &moderation.SubjectRefView{URI: action.SubjectURI, CID: action.ObservedCID}, admin.PrivateSubject)
		})
	}
	public := modlogTestAction()
	public.ScopeCommunityDID = "did:plc:scopecommunity"
	view := moderation.NewActionView(public)
	assert.Equal(t, &moderation.SubjectRefView{URI: public.SubjectURI, CID: public.ObservedCID}, view.Subject)
	assert.Equal(t, public.ScopeCommunityDID, view.Scope.CommunityDID)
	assert.Nil(t, moderation.NewAdminActionView(public).PrivateSubject)
}

// SQL and Go must agree on restricted subjects. A restricted row returned under
// ExcludeRestricted means they disagree, and the public page fails closed.
func TestModlogListFailsClosedWhenStoreReturnsRestrictedRowUnderExclusion(t *testing.T) {
	for _, lookAhead := range []bool{false, true} {
		t.Run(fmt.Sprintf("look-ahead=%t", lookAhead), func(t *testing.T) {
			rows := modlogCursorRows()
			index := 0
			if lookAhead {
				index = 2 // The unprojected look-ahead row still proves the disagreement.
			}
			rows[index].SubjectAccess = moderation.SubjectAccessRestricted
			store := newInMemoryModerationStore(time.Now())
			store.listRows = rows
			service := modlogFilterService(store, nil, nil)
			params := moderation.ListActionsParams{Limit: modlogLimit(2), Community: "did:plc:community"}
			page, err := modlogFilterList(t, service, false, params, "")
			require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
			assert.Nil(t, page)

			unfiltered, err := service.ListActions(t.Context(), moderation.ListActionsParams{Limit: modlogLimit(2)})
			require.NoError(t, err, "an unfiltered public page projects restricted rows without their subject")
			if !lookAhead {
				assert.Nil(t, unfiltered.Actions[0].Subject)
			}
			_, err = modlogFilterList(t, service, true, params, "")
			require.NoError(t, err, "the admin log never excludes restricted rows")
		})
	}
}

// Every listed row must say whether its subject is public. A row without that
// state cannot be projected safely, on either log.
func TestModlogListFailsClosedOnUndecidedSubjectAccess(t *testing.T) {
	for _, access := range []moderation.SubjectAccess{"", "unknown"} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/admin=%t", access, admin), func(t *testing.T) {
				rows := modlogCursorRows()
				rows[2].SubjectAccess = access
				store := newInMemoryModerationStore(time.Now())
				store.listRows = rows
				page, err := modlogFilterList(t, modlogFilterService(store, nil, nil), admin, moderation.ListActionsParams{}, "")
				require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
				assert.Nil(t, page)
			})
		}
	}
}
