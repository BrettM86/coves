//go:build integration

package jetstream

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostV2Consumer_ContentAndTitleCapsApplyToCreateAndUpdate(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	ctx := context.Background()

	oversizedCreates := []struct {
		name    string
		rkey    string
		title   string
		content string
	}{
		{
			name:    "content over 100000 bytes",
			rkey:    "pv2contentovercap",
			title:   "oversized content",
			content: strings.Repeat("c", 100_001),
		},
		{
			name:    "title over 3000 bytes",
			rkey:    "pv2titleovercap",
			title:   strings.Repeat("t", 3_001),
			content: "valid content",
		},
	}
	for _, testCase := range oversizedCreates {
		t.Run("create/"+testCase.name, func(t *testing.T) {
			uri := pv2URI(pv2Author, testCase.rkey)
			err := f.consumer.HandleEvent(ctx, pv2Event(
				pv2Author, "create", testCase.rkey, testkit.TID(), "bafyreicap"+testCase.rkey,
				time.Now().UnixMicro(), pv2Record(pv2Community, testCase.title, testCase.content),
			))

			assert.Truef(t, errors.Is(err, ErrPermanentEvent),
				"a postv2 create with %s is permanently invalid payload, not a retryable infrastructure failure; got %v", testCase.name, err)
			assert.ErrorContainsf(t, err, "exceeds maximum length",
				"the permanent rejection for %s must name the violated byte cap so dead-letter operators can diagnose it", testCase.name)
			assert.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri),
				"a postv2 create rejected for "+testCase.name+" must not leave a posts row behind")
		})
	}

	t.Run("update/content over 100000 bytes", func(t *testing.T) {
		const (
			rkey            = "pv2updatecontentovercap"
			originalContent = "content before the oversized update"
		)
		uri := pv2URI(pv2Author, rkey)
		revs := increasingTIDs(t, 2)
		base := time.Now().UnixMicro()
		require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
			pv2Author, "create", rkey, revs[0], "bafyreicapupdatev1", base,
			pv2Record(pv2Community, "original title", originalContent),
		)))

		err := f.consumer.HandleEvent(ctx, pv2Event(
			pv2Author, "update", rkey, revs[1], "bafyreicapupdatev2", base+1_000_000,
			pv2Record(pv2Community, "updated title", strings.Repeat("u", 100_001)),
		))

		assert.Truef(t, errors.Is(err, ErrPermanentEvent),
			"a postv2 update with content over 100,000 bytes must return ErrPermanentEvent; got %v", err)
		assert.ErrorContains(t, err, "exceeds maximum length",
			"the oversized update rejection must name the content cap")
		var contentUnchanged bool
		require.NoError(t, db.QueryRow(
			`SELECT content = $2 FROM posts WHERE uri = $1`, uri, originalContent,
		).Scan(&contentUnchanged))
		assert.True(t, contentUnchanged,
			"rejecting oversized update content must leave the previously indexed content unchanged")
	})

	acceptedAtCaps := []struct {
		name        string
		rkey        string
		title       string
		content     string
		wantTitle   int
		wantContent int
	}{
		{
			name:        "content at 100000 bytes",
			rkey:        "pv2contentatcap",
			title:       "content boundary",
			content:     strings.Repeat("c", 100_000),
			wantTitle:   len("content boundary"),
			wantContent: 100_000,
		},
		{
			name:        "title at 3000 bytes",
			rkey:        "pv2titleatcap",
			title:       strings.Repeat("t", 3_000),
			content:     "title boundary",
			wantTitle:   3_000,
			wantContent: len("title boundary"),
		},
	}
	for _, testCase := range acceptedAtCaps {
		t.Run("create/"+testCase.name, func(t *testing.T) {
			uri := pv2URI(pv2Author, testCase.rkey)
			require.NoErrorf(t, f.consumer.HandleEvent(ctx, pv2Event(
				pv2Author, "create", testCase.rkey, testkit.TID(), "bafyreicap"+testCase.rkey,
				time.Now().UnixMicro(), pv2Record(pv2Community, testCase.title, testCase.content),
			)), "the postv2 consumer must accept %s exactly at the byte cap", testCase.name)

			row := readPostV2MechanismRow(t, db, uri)
			assert.Lenf(t, row.Title, testCase.wantTitle,
				"the accepted %s title must be stored without truncation", testCase.name)
			assert.Lenf(t, row.Content, testCase.wantContent,
				"the accepted %s content must be stored without truncation", testCase.name)
		})
	}
}
