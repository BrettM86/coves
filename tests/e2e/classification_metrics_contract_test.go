//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const getClassificationMetricsMethod = "social.coves.moderation.getClassificationMetrics"

// classificationMetricsSteadyWindow covers several classifier worker
// intervals (2s in .env.ci), so a job total that holds over it is a baseline
// no earlier test's indexing is still adding to.
const classificationMetricsSteadyWindow = 8 * time.Second

// classificationMetricsTestJobs is the number of jobs the test's writes
// enqueue: the community, the plain post, the comment on it, the edited
// post's two CIDs and the deleted post.
const classificationMetricsTestJobs = 6

type classificationJobCounts struct {
	Queued           int64 `json:"queued"`
	Deferred         int64 `json:"deferred"`
	Retrying         int64 `json:"retrying"`
	Sent             int64 `json:"sent"`
	Failed           int64 `json:"failed"`
	ClosedSuperseded int64 `json:"closedSuperseded"`
	ClosedNotVisible int64 `json:"closedNotVisible"`
}

// total counts every job the metrics report. The worker never creates or
// removes a job, it only moves one between statuses, so on the shared stack,
// where earlier tests' jobs keep retrying, failing and closing, the total
// moves only when a job is enqueued (or an account erasure deletes jobs).
func (c classificationJobCounts) total() int64 {
	return c.Queued + c.Deferred + c.Retrying + c.Sent + c.Failed + c.ClosedSuperseded + c.ClosedNotVisible
}

type classificationMetricsResponse struct {
	Jobs classificationJobCounts `json:"jobs"`
}

// TestClassificationMetricsAPIContract: on the hermetic stack with the
// classifier configured (egress blocked, so no job is ever classified) and
// T2 running serially, an admin's metrics count exactly the six jobs the
// test's writes enqueue (a community, a plain post, a comment on it, an
// edited post's two CIDs, a deleted post) and at least one more superseded
// close and one more not-visible close, for the edit's first CID and the
// deleted post; the body is
// lexicon-valid; a non-admin is forbidden (B11). The stack is shared and
// earlier tests' jobs keep moving (retrying, reaching the attempt limit,
// closing when their subject went away), and the metrics cannot be scoped to
// one subject, so the test asserts the job total, which only enqueues move,
// exactly, and the close counts only as lower bounds.
func TestClassificationMetricsAPIContract(t *testing.T) {
	p := newPipeline(t)
	admin := testkit.ModerationAdmin(t, 1)
	nonAdmin := p.IndexedAccount(t, "clsmetoutsider")
	author := p.IndexedAccount(t, "clsmetauthor")

	readMetrics := func() (classificationMetricsResponse, json.RawMessage, error) {
		token := admin.ServiceAuth(t, communityInstanceDID, getClassificationMetricsMethod)
		var body json.RawMessage
		if err := p.AppView.As(token).Query(context.Background(), getClassificationMetricsMethod, nil, &body); err != nil {
			return classificationMetricsResponse{}, nil, err
		}
		var metrics classificationMetricsResponse
		if err := json.Unmarshal(body, &metrics); err != nil {
			return classificationMetricsResponse{}, nil, err
		}
		return metrics, body, nil
	}

	// Baseline: two consecutive reads one worker interval apart agree on the
	// job total, then it holds over several worker intervals. The per-status
	// counts are not required to hold: earlier tests' jobs move between them.
	var baseline classificationJobCounts
	var previousTotal *int64
	testkit.WaitFor(t, contractBudget, func() (bool, error) {
		metrics, _, err := readMetrics()
		if err != nil {
			return false, err
		}
		total := metrics.Jobs.total()
		steady := previousTotal != nil && *previousTotal == total
		previousTotal = &total
		baseline = metrics.Jobs
		return steady, nil
	}, testkit.WithPollInterval(2*time.Second), testkit.WithDescription("the classification job total to stop moving"))
	testkit.Holds(t, classificationMetricsSteadyWindow, func() (bool, error) {
		metrics, _, err := readMetrics()
		if err != nil {
			return false, err
		}
		return metrics.Jobs.total() == baseline.total(), nil
	}, testkit.WithPollInterval(time.Second), testkit.WithDescription("the baseline job total to hold steady"))

	// The community is created after the baseline so its own job, enqueued
	// by the community repository, is one of the counted ones.
	community := indexedCommunity(t, p, "cmt", author.DID)

	// Post 1: written and indexed, its job stays pending.
	plain := indexedPost(t, p, community, author, "metrics plain "+testkit.UniqueID(t))

	// A comment on post 1: written and indexed, its job, enqueued by the
	// comment consumer, stays pending.
	commentRkey := testkit.TID()
	commentAtURI := commentURI(author.DID, commentRkey)
	author.PutRecord(t, commentCollection, commentRkey,
		commentRecord(plain, plain, "metrics comment "+testkit.UniqueID(t)))
	p.Await(t, "the comment to be served on the plain post", func() (bool, error) {
		thread, err := p.Thread(context.Background(), plain.URI, nil)
		if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
			return done, err
		}
		_, ok := thread.find(commentAtURI)
		return ok, nil
	})

	// Post 2: written, indexed, then edited; the first CID's job closes
	// superseded and the new CID's job stays pending.
	editedRkey := testkit.TID()
	editedURI := authorPostURI(author.DID, editedRkey)
	original := author.PutRecord(t, postV2Collection, editedRkey,
		postV2Record(community.DID, "metrics edited "+testkit.UniqueID(t), "the first version"))
	awaitStatus(t, p, editedURI, community.DID, "pending", "the post to be edited to reach the admission queue")
	community.PutRecord(t, acceptanceCollection, subjectRkey(editedURI), acceptanceRecord(editedURI, original.CID))
	awaitStatus(t, p, editedURI, community.DID, "accepted", "the post to be edited to be accepted")
	edited := author.PutRecord(t, postV2Collection, editedRkey,
		postV2Record(community.DID, "metrics edited again "+testkit.UniqueID(t), "the second version"))
	require.NotEqual(t, original.CID, edited.CID, "the edit must produce a new CID")
	community.PutRecord(t, acceptanceCollection, subjectRkey(editedURI), acceptanceRecord(editedURI, edited.CID))
	p.Await(t, "the edited post to be served at its new CID", func() (bool, error) {
		view, err := p.Post(context.Background(), editedURI)
		if err != nil {
			return false, err
		}
		return !view.NotFound && view.CID == edited.CID, nil
	})

	// Post 3: written, indexed, then deleted; its job closes not visible.
	deletedRkey := testkit.TID()
	deletedURI := authorPostURI(author.DID, deletedRkey)
	deleted := author.PutRecord(t, postV2Collection, deletedRkey,
		postV2Record(community.DID, "metrics deleted "+testkit.UniqueID(t), "a post that is deleted"))
	awaitStatus(t, p, deletedURI, community.DID, "pending", "the post to be deleted to reach the admission queue")
	community.PutRecord(t, acceptanceCollection, subjectRkey(deletedURI), acceptanceRecord(deletedURI, deleted.CID))
	awaitStatus(t, p, deletedURI, community.DID, "accepted", "the post to be deleted to be accepted")
	p.Await(t, "the post to be deleted to be served", func() (bool, error) {
		view, err := p.Post(context.Background(), deletedURI)
		if err != nil {
			return false, err
		}
		return !view.NotFound, nil
	})
	author.DeleteExistingRecord(t, postV2Collection, deletedRkey)
	p.Await(t, "the deleted post to stop being served", func() (bool, error) {
		view, err := p.Post(context.Background(), deletedURI)
		if err != nil {
			return false, err
		}
		return view.NotFound, nil
	})

	var (
		final     classificationJobCounts
		finalBody json.RawMessage
	)
	p.Await(t, "the test's six jobs, the edited post's superseded close and the deleted post's not-visible close to be counted", func() (bool, error) {
		metrics, body, err := readMetrics()
		if err != nil {
			return false, err
		}
		final, finalBody = metrics.Jobs, body
		return metrics.Jobs.total() >= baseline.total()+classificationMetricsTestJobs &&
			metrics.Jobs.ClosedSuperseded >= baseline.ClosedSuperseded+1 &&
			metrics.Jobs.ClosedNotVisible >= baseline.ClosedNotVisible+1, nil
	})

	assert.Equal(t, baseline.total()+classificationMetricsTestJobs, final.total(),
		"exactly the test's six jobs are added: one each for the community, the plain post, the comment, the edit's two CIDs and the deleted post")
	assert.Equal(t, int64(0), final.Sent, "with egress blocked no job is classified")
	requireClassificationLexiconOutput(t, finalBody, getClassificationMetricsMethod)

	t.Run("non-admin metrics is forbidden", func(t *testing.T) {
		token := nonAdmin.ServiceAuth(t, communityInstanceDID, getClassificationMetricsMethod)
		err := p.AppView.As(token).Query(t.Context(), getClassificationMetricsMethod, nil, nil)
		_ = requireXRPCRefusal(t, err, http.StatusForbidden, "Forbidden", "a non-admin's metrics call")
	})
}
