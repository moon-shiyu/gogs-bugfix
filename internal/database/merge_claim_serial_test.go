package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMergeClaimSerialization simulates two merge requests arriving for the
// same tips. The unique idempotency key guarantees only one can be held, the
// second observes an in-progress attempt instead of writing again.
func TestMergeClaimSerialization(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	setupMergeIntegrationEngine(t)

	pr := &PullRequest{
		ID:           100,
		Status:       PullRequestStatusMergeable,
		HeadUserName: "u",
		HeadRepo:     &Repository{Name: "h"},
		BaseRepo:     &Repository{Name: "b"},
		HeadBranch:   "f",
		BaseBranch:   "main",
	}
	_, err := x.Insert(pr)
	require.NoError(t, err)

	doer := &User{ID: 1}
	guard := &mergeGuard{headSHA: "aaa", baseSHA: "bbb"}
	opts := MergePullRequestOptions{PullRequest: pr, Doer: doer}

	first, err := claimMergeRequest(t.Context(), pr, guard, opts)
	require.NoError(t, err)
	assert.True(t, first.held)
	assert.Equal(t, MergeRequestStatePending, first.State)

	second, err := claimMergeRequest(t.Context(), pr, guard, opts)
	require.NoError(t, err)
	assert.False(t, second.held)
	assert.Equal(t, MergeRequestStatePending, second.State)
	assert.Equal(t, first.ID, second.ID)

	// A request captured after the target branch moved must not be replayed.
	movedGuard := &mergeGuard{headSHA: "aaa", baseSHA: "ccc"}
	_, err = claimMergeRequest(t.Context(), pr, movedGuard, opts)
	assert.True(t, IsErrMergeRequestChanged(err))
}
