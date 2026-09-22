package database

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluateRequiredChecks(t *testing.T) {
	required := []string{"ci/build", "ci/lint", "ci/test"}

	t.Run("all required checks pass", func(t *testing.T) {
		checks := []*StatusCheck{
			{Name: "ci/build", State: StatusCheckStateSuccess},
			{Name: "ci/lint", State: StatusCheckStateSuccess},
			{Name: "ci/test", State: StatusCheckStateSuccess},
			{Name: "optional/scan", State: StatusCheckStateFailure},
		}
		require.NoError(t, evaluateRequiredChecks(required, checks))
	})

	t.Run("missing result lists the exact check name", func(t *testing.T) {
		checks := []*StatusCheck{
			{Name: "ci/build", State: StatusCheckStateSuccess},
			{Name: "ci/lint", State: StatusCheckStateSuccess},
		}
		err := evaluateRequiredChecks(required, checks)
		var requiredErr ErrRequiredChecksMissing
		require.ErrorAs(t, err, &requiredErr)
		assert.Equal(t, []string{"ci/test"}, requiredErr.Missing)
		assert.Contains(t, err.Error(), "ci/test")
	})

	t.Run("pending result is not a pass", func(t *testing.T) {
		checks := []*StatusCheck{
			{Name: "ci/build", State: StatusCheckStateSuccess},
			{Name: "ci/lint", State: StatusCheckStatePending},
			{Name: "ci/test", State: StatusCheckStateSuccess},
		}
		err := evaluateRequiredChecks(required, checks)
		var requiredErr ErrRequiredChecksMissing
		require.ErrorAs(t, err, &requiredErr)
		assert.Equal(t, []string{"ci/lint"}, requiredErr.Pending)
	})

	t.Run("failure result is reported as failed", func(t *testing.T) {
		checks := []*StatusCheck{
			{Name: "ci/build", State: StatusCheckStateSuccess},
			{Name: "ci/lint", State: StatusCheckStateError},
			{Name: "ci/test", State: StatusCheckStateSuccess},
		}
		err := evaluateRequiredChecks(required, checks)
		var requiredErr ErrRequiredChecksMissing
		require.ErrorAs(t, err, &requiredErr)
		assert.Equal(t, []string{"ci/lint"}, requiredErr.Failed)
	})

	t.Run("old success on a different base tip does not pass", func(t *testing.T) {
		// The filter in merge readiness drops stale checks before evaluation,
		// simulate that by passing only the valid subset.
		checks := []*StatusCheck{
			{Name: "ci/build", State: StatusCheckStateSuccess, BaseSHA: "new"},
			{Name: "ci/lint", State: StatusCheckStateSuccess, BaseSHA: "new"},
		}
		err := evaluateRequiredChecks(required, checks)
		var requiredErr ErrRequiredChecksMissing
		require.ErrorAs(t, err, &requiredErr)
		assert.Equal(t, []string{"ci/test"}, requiredErr.Missing)
	})

	t.Run("no required checks means no gate", func(t *testing.T) {
		require.NoError(t, evaluateRequiredChecks(nil, nil))
	})
}

func TestStatusChecksStore_CommitScoping(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	t.Parallel()

	ctx := context.Background()
	store := &StatusChecksStore{db: newTestDB(t, "StatusChecksStore")}

	check, err := store.SetStatusCheck(ctx, SetStatusCheckOptions{
		RepoID:      1,
		CommitSHA:   "AAA111",
		BaseSHA:     "BBB111",
		RuleVersion: 1,
		Name:        "ci/build",
		State:       "success",
	})
	require.NoError(t, err)
	assert.NotZero(t, check.ID)

	// Same name on a new head commit is a separate row, the old success can not
	// authorize a merge of the new commit.
	_, err = store.SetStatusCheck(ctx, SetStatusCheckOptions{
		RepoID:      1,
		CommitSHA:   "AAA222",
		BaseSHA:     "BBB111",
		RuleVersion: 1,
		Name:        "ci/build",
		State:       "pending",
	})
	require.NoError(t, err)

	newCommitChecks, err := store.ListByCommit(ctx, 1, "aaa222")
	require.NoError(t, err)
	require.Len(t, newCommitChecks, 1)
	assert.Equal(t, StatusCheckStatePending, newCommitChecks[0].State)

	// Same name against a moved base tip on the same head commit is also a new
	// row, stale results can not be reused.
	_, err = store.SetStatusCheck(ctx, SetStatusCheckOptions{
		RepoID:      1,
		CommitSHA:   "AAA111",
		BaseSHA:     "BBB222",
		RuleVersion: 1,
		Name:        "ci/build",
		State:       "pending",
	})
	require.NoError(t, err)
	oldCommitChecks, err := store.ListByCommit(ctx, 1, "AAA111")
	require.NoError(t, err)
	assert.Len(t, oldCommitChecks, 2)

	// A rule version bump produces yet another independent result row.
	_, err = store.SetStatusCheck(ctx, SetStatusCheckOptions{
		RepoID:      1,
		CommitSHA:   "AAA111",
		BaseSHA:     "BBB111",
		RuleVersion: 2,
		Name:        "ci/build",
		State:       "pending",
	})
	require.NoError(t, err)
	oldCommitChecks, err = store.ListByCommit(ctx, 1, "AAA111")
	require.NoError(t, err)
	assert.Len(t, oldCommitChecks, 3)
}

func TestParseRequiredStatusCheckNames(t *testing.T) {
	assert.Equal(t, []string{"a", "b", "c"}, ParseRequiredStatusCheckNames("c, a ,b,, a"))
	assert.Nil(t, ParseRequiredStatusCheckNames(""))
	assert.Nil(t, ParseRequiredStatusCheckNames("  , "))
}

func TestFilterCurrentChecks(t *testing.T) {
	checks := []*StatusCheck{
		{Name: "ci/a", State: StatusCheckStateSuccess, BaseSHA: "new", RuleVersion: 2},
		{Name: "ci/b", State: StatusCheckStateSuccess, BaseSHA: "old", RuleVersion: 2},
		{Name: "ci/c", State: StatusCheckStateSuccess, BaseSHA: "new", RuleVersion: 1},
	}
	valid, stale := filterCurrentChecks(checks, "new", 2)
	require.Len(t, valid, 1)
	assert.Equal(t, "ci/a", valid[0].Name)
	assert.True(t, stale)
}

func TestApplyCheckRollup(t *testing.T) {
	t.Run("stale green on new history shows waiting not green", func(t *testing.T) {
		status := &PullRequestCheckStatus{
			Required: []string{"ci/a"},
		}
		staleChecks := []*StatusCheck{
			{Name: "ci/a", State: StatusCheckStateSuccess, BaseSHA: "old", RuleVersion: 1},
		}
		valid, hasStale := filterCurrentChecks(staleChecks, "new", 2)
		applyCheckRollup(status, valid, hasStale, PullRequestStatusMergeable)
		assert.Equal(t, CheckRollupStatePending, status.State)
		assert.Equal(t, []string{"ci/a"}, status.Missing)
		assert.NotEmpty(t, status.StaleReason)
	})

	t.Run("all fresh and passing shows success", func(t *testing.T) {
		status := &PullRequestCheckStatus{
			Required: []string{"ci/a"},
		}
		applyCheckRollup(status, []*StatusCheck{
			{Name: "ci/a", State: StatusCheckStateSuccess, BaseSHA: "new", RuleVersion: 2},
		}, false, PullRequestStatusMergeable)
		assert.Equal(t, CheckRollupStateSuccess, status.State)
	})

	t.Run("fresh failure shows failure", func(t *testing.T) {
		status := &PullRequestCheckStatus{
			Required: []string{"ci/a"},
		}
		applyCheckRollup(status, []*StatusCheck{
			{Name: "ci/a", State: StatusCheckStateFailure, BaseSHA: "new", RuleVersion: 2},
		}, false, PullRequestStatusMergeable)
		assert.Equal(t, CheckRollupStateFailure, status.State)
		assert.Equal(t, []string{"ci/a"}, status.Failed)
	})

	t.Run("pending result shows waiting", func(t *testing.T) {
		status := &PullRequestCheckStatus{
			Required: []string{"ci/a"},
		}
		applyCheckRollup(status, []*StatusCheck{
			{Name: "ci/a", State: StatusCheckStatePending, BaseSHA: "new", RuleVersion: 2},
		}, false, PullRequestStatusMergeable)
		assert.Equal(t, CheckRollupStatePending, status.State)
		assert.Equal(t, []string{"ci/a"}, status.Pending)
	})
}
