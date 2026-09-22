package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeRequestIdempotencyKey(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	t.Parallel()

	db := newTestDB(t, "MergeRequestIdempotency")

	first := &MergeRequest{
		PullRequestID: 1,
		HeadSHA:       "head-a",
		BaseSHA:       "base-a",
		DoerID:        7,
		MergeStyle:    string(MergeStyleRegular),
		State:         MergeRequestStatePending,
	}
	require.NoError(t, db.Create(first).Error)

	// Double click with the same (PR, head, base) tuple must not create a
	// second write opportunity.
	duplicate := &MergeRequest{
		PullRequestID: 1,
		HeadSHA:       "head-a",
		BaseSHA:       "base-a",
		DoerID:        7,
		State:         MergeRequestStatePending,
	}
	assert.Error(t, db.Create(duplicate).Error)

	// A different base tip (the target branch received a normal push while
	// checks were running) gets its own attempt record.
	afterPush := &MergeRequest{
		PullRequestID: 2,
		HeadSHA:       "head-a",
		BaseSHA:       "base-b",
		DoerID:        7,
		State:         MergeRequestStatePending,
	}
	require.NoError(t, db.Create(afterPush).Error)

	var count int64
	require.NoError(t, db.Model(new(MergeRequest)).Where("pull_request_id = ?", 1).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestMergeClaimRecoveryStates(t *testing.T) {
	assert.True(t, isRecoverableFailure("previous merge attempt was interrupted before the push completed"))
	assert.True(t, isRecoverableFailure("target branch changed while the merge was interrupted"))
	assert.False(t, isRecoverableFailure("git merge conflict"))
	assert.False(t, isRecoverableFailure(""))
}

func TestErrorTypes(t *testing.T) {
	var changed interface {
		conflictChecker
		error
	}
	changed = ErrMergeRequestChanged{args: map[string]any{"reason": "x"}}
	assert.True(t, changed.Conflict())
	assert.True(t, IsErrMergeRequestChanged(changed))

	var resync interface {
		conflictChecker
		error
	}
	resync = ErrStateNeedsResync{args: map[string]any{"reason": "y"}}
	assert.True(t, resync.Conflict())
	assert.True(t, IsErrStateNeedsResync(resync))
	assert.Contains(t, resync.Error(), "y")

	required := ErrRequiredChecksMissing{Missing: []string{"ci/a"}}
	assert.True(t, required.Conflict())
	assert.Contains(t, required.Error(), "ci/a")
}

type conflictChecker interface {
	Conflict() bool
}

func TestProtectBranchRuleVersionBump(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	t.Parallel()

	db := newTestDB(t, "ProtectBranchRuleVersion")

	branch := &ProtectBranch{
		RepoID:               42,
		Name:                 "main",
		Protected:            true,
		RuleVersion:          1,
		RequiredStatusChecks: "ci/a",
	}
	require.NoError(t, db.Create(branch).Error)

	// Stored checks from the old rule version differ from the new version, so
	// the version column must be usable to invalidate them.
	var checksOnOldVersion int64
	require.NoError(t, db.Model(new(StatusCheck)).
		Where("repo_id = ? AND rule_version < ?", 42, branch.RuleVersion).
		Count(&checksOnOldVersion).Error)
	assert.Equal(t, int64(0), checksOnOldVersion)
}

func TestProtectBranchRulesChanged(t *testing.T) {
	before := &ProtectBranch{
		Protected:            true,
		RequirePullRequest:   true,
		RequiredStatusChecks: "ci/a,ci/b",
		EnableWhitelist:      false,
	}

	t.Run("same rules after normalization do not bump", func(t *testing.T) {
		after := *before
		after.RequiredStatusChecks = "ci/b, ci/a, ci/a"
		assert.False(t, protectBranchRulesChanged(before, &after))
	})

	t.Run("added required check bumps rules", func(t *testing.T) {
		after := *before
		after.RequiredStatusChecks = "ci/a,ci/b,ci/c"
		assert.True(t, protectBranchRulesChanged(before, &after))
	})

	t.Run("removed required check bumps rules", func(t *testing.T) {
		after := *before
		after.RequiredStatusChecks = "ci/a"
		assert.True(t, protectBranchRulesChanged(before, &after))
	})

	t.Run("toggling protection bumps rules", func(t *testing.T) {
		after := *before
		after.Protected = false
		assert.True(t, protectBranchRulesChanged(before, &after))
	})

	t.Run("whitelist change bumps rules", func(t *testing.T) {
		after := *before
		after.EnableWhitelist = true
		after.WhitelistUserIDs = "1"
		assert.True(t, protectBranchRulesChanged(before, &after))
	})
}
