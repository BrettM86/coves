package moderation_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NSFW label applies and retractions are left out of the public modlog
// (user decision, 2026-09-30). The admin log keeps them.

func TestModlogPublicRejectsLabelActionFilters(t *testing.T) {
	for _, action := range []string{moderation.ActionLabel, moderation.ActionRetractLabel} {
		t.Run(action+"/public", func(t *testing.T) {
			store := newInMemoryModerationStore(time.Now())
			page, err := modlogFilterList(t, modlogFilterService(store, nil, nil), false, moderation.ListActionsParams{Action: action}, "")
			require.ErrorIs(t, err, moderation.ErrInvalidRequest)
			assert.Nil(t, page)
			assert.Empty(t, store.listQueries, "a refused public filter must not reach the store")
		})
		t.Run(action+"/admin", func(t *testing.T) {
			store := newInMemoryModerationStore(time.Now())
			_, err := modlogFilterList(t, modlogFilterService(store, nil, nil), true, moderation.ListActionsParams{Action: action}, "")
			require.NoError(t, err)
			require.Len(t, store.listQueries, 1)
			assert.Equal(t, action, store.listQueries[0].Action)
			assert.False(t, store.listQueries[0].ExcludeLabelActions)
		})
	}
}

func TestModlogExcludeLabelActionsOnPublicQueriesOnly(t *testing.T) {
	for _, test := range []struct {
		name   string
		params moderation.ListActionsParams
	}{
		{"unfiltered", moderation.ListActionsParams{}},
		{"subject", moderation.ListActionsParams{Subject: modlogTestAction().SubjectURI}},
		{"collection", moderation.ListActionsParams{Collection: moderation.PostV2Collection}},
		{"community", moderation.ListActionsParams{Community: "did:plc:community"}},
		{"actor", moderation.ListActionsParams{Actor: "did:plc:actor"}},
		{"authority", moderation.ListActionsParams{Authority: "did:plc:authority"}},
		{"action", moderation.ListActionsParams{Action: moderation.ActionRemove}},
		{"origin", moderation.ListActionsParams{Origin: moderation.OriginLocal}},
		{"time range", moderation.ListActionsParams{Since: "2026-09-28T12:00:00Z", Until: "2026-09-29T12:00:00Z"}},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				store := newInMemoryModerationStore(time.Now())
				store.listRows = modlogCursorRows()
				service := modlogFilterService(store, nil, nil)
				params := test.params
				params.Limit = modlogLimit(1)
				page, err := modlogFilterList(t, service, admin, params, "")
				require.NoError(t, err)
				params.Cursor = modlogPageCursor(t, page)
				require.NotEmpty(t, params.Cursor)
				_, err = modlogFilterList(t, service, admin, params, "")
				require.NoError(t, err)
				require.Len(t, store.listQueries, 2)
				for index, query := range store.listQueries {
					assert.Equal(t, !admin, query.ExcludeLabelActions, "query %d", index)
				}
				require.NotNil(t, store.listQueries[1].Before, "the second query must be a cursor page")
			})
		}
	}
}

// labelLeakingStore returns its rows whatever the query asks, standing in for
// SQL that disagrees with the Go rule.
type labelLeakingStore struct {
	*inMemoryModerationStore
	rows []moderation.Action
}

func (store *labelLeakingStore) ListActions(context.Context, moderation.ActionListQuery) ([]moderation.Action, error) {
	return append([]moderation.Action(nil), store.rows...), nil
}

// SQL and Go must agree on the excluded kinds. A label row returned under
// ExcludeLabelActions means they disagree, and the public page fails closed.
func TestModlogListFailsClosedWhenStoreReturnsLabelActionPublicly(t *testing.T) {
	for _, kind := range []string{moderation.ActionLabel, moderation.ActionRetractLabel} {
		for _, lookAhead := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/look-ahead=%t", kind, lookAhead), func(t *testing.T) {
				rows := modlogCursorRows()
				index := 0
				if lookAhead {
					index = 2 // The unprojected look-ahead row still proves the disagreement.
				}
				rows[index].Action = kind
				rows[index].LabelValue = moderation.LabelNSFW
				store := &labelLeakingStore{inMemoryModerationStore: newInMemoryModerationStore(time.Now()), rows: rows}
				service := moderation.NewService(&fakeSubjectReader{}, store,
					moderation.Config{InstanceDID: "did:web:instance.example", CursorSecret: "secret-one"})
				for _, params := range []moderation.ListActionsParams{
					{Limit: modlogLimit(2)},
					{Limit: modlogLimit(2), Subject: modlogTestAction().SubjectURI},
				} {
					page, err := modlogFilterList(t, service, false, params, "")
					require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
					assert.Nil(t, page)
					_, err = modlogFilterList(t, service, true, params, "")
					require.NoError(t, err, "the admin log serves label actions")
				}
			})
		}
	}
}
