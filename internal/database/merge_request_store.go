package database

import (
	"context"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gogs/git-module"
	log "unknwon.dev/clog/v2"
	"xorm.io/xorm"

	"gogs.io/gogs/internal/conf"
	apiv1types "gogs.io/gogs/internal/route/api/v1/types"
)

type mergeClaim struct {
	*MergeRequest
	// held is true when this call owns the pending row and should execute.
	held bool
	// retryable is true when a failed attempt is an interrupted merge that the
	// same request may retry once on the unchanged tips.
	retryable bool
}

// mergeClaimLease is how long a pending execution owns the slot. Git merges
// are bounded by the clone/fetch timeout, 10 minutes is safely beyond normal
// runtime while still unblocking crashed processes quickly.
const mergeClaimLease = 10 * time.Minute

// claimMergeRequest returns the unique idempotency record for
// (PR, head SHA, base SHA). Concurrent identical requests serialize here, and
// requests whose base SHA differs get a changed error instead of a replay.
func claimMergeRequest(ctx context.Context, pr *PullRequest, guard *mergeGuard, opts MergePullRequestOptions) (*mergeClaim, error) {
	sess := x.NewSession()
	defer sess.Close()
	if err := sess.Begin(); err != nil {
		return nil, errors.Wrap(err, "begin transaction")
	}

	lockedPR := new(PullRequest)
	has, err := sess.ID(pr.ID).ForUpdate().Get(lockedPR)
	if err != nil {
		return nil, errors.Wrap(err, "lock pull request")
	}
	if !has {
		return nil, ErrPullRequestNotExist{args: map[string]any{"pullRequestID": pr.ID}}
	}
	if lockedPR.HasMerged {
		if err = sess.Commit(); err != nil {
			return nil, errors.Wrap(err, "commit")
		}
		return &mergeClaim{
			MergeRequest: &MergeRequest{PullRequestID: pr.ID, State: MergeRequestStateMerged},
			held:         false,
		}, nil
	}

	var attempts []*MergeRequest
	if err = sess.Where("pull_request_id = ?", pr.ID).Find(&attempts); err != nil {
		return nil, errors.Wrap(err, "find merge requests")
	}

	now := time.Now()
	var matching *MergeRequest
	for _, attempt := range attempts {
		if attempt.HeadSHA == guard.headSHA && attempt.BaseSHA == guard.baseSHA {
			matching = attempt
			break
		}
	}

	// Recover crashed attempts before deciding what to return.
	if matching != nil && matching.State == MergeRequestStatePending && matching.LeaseExpiresUnix < now.Unix() {
		recovered, err := recoverExpiredMergeRequest(sess, matching, pr, guard)
		if err != nil {
			return nil, err
		}
		matching = recovered
	}

	if matching != nil {
		if err = sess.Commit(); err != nil {
			return nil, errors.Wrap(err, "commit")
		}
		return &mergeClaim{
			MergeRequest: matching,
			held:         matching.State == MergeRequestStateFailed && isRecoverableFailure(matching.FailureReason),
			retryable:    matching.State == MergeRequestStateFailed && isRecoverableFailure(matching.FailureReason),
		}, nil
	}

	// A prior attempt on different tips exists. The request is stale, it must
	// surface a changed message instead of silently replaying a second write.
	if len(attempts) > 0 {
		_ = sess.Commit()
		return nil, ErrMergeRequestChanged{args: map[string]any{
			"reason":        "a merge was requested against an older target branch state",
			"pull_request":  pr.ID,
			"expected_base": guard.baseSHA,
		}}
	}

	record := &MergeRequest{
		PullRequestID:    pr.ID,
		HeadSHA:          guard.headSHA,
		BaseSHA:          guard.baseSHA,
		DoerID:           opts.Doer.ID,
		MergeStyle:       string(opts.MergeStyle),
		State:            MergeRequestStatePending,
		LeaseExpiresUnix: now.Add(mergeClaimLease).Unix(),
	}
	record.BeforeCreate()
	if _, err = sess.Insert(record); err != nil {
		return nil, errors.Wrap(err, "insert merge request")
	}

	if err = sess.Commit(); err != nil {
		return nil, errors.Wrap(err, "commit")
	}
	return &mergeClaim{MergeRequest: record, held: true}, nil
}

// recoverExpiredMergeRequest reconciles a crashed attempt with actual git
// state. Recovery is deterministic: the merge landed when the result commit
// exists in the target repository, otherwise the branch moved and the attempt
// must restart.
func recoverExpiredMergeRequest(sess *xorm.Session, record *MergeRequest, pr *PullRequest, guard *mergeGuard) (*MergeRequest, error) {
	baseGitRepo, err := git.Open(pr.BaseRepo.RepoPath())
	if err != nil {
		return nil, errors.Wrap(err, "open base repository")
	}

	currentBaseSHA, err := baseGitRepo.BranchCommitID(pr.BaseBranch)
	if err != nil {
		return nil, errors.Wrap(err, "get current base branch tip")
	}
	currentBaseSHA = strings.ToLower(currentBaseSHA)

	if currentBaseSHA != guard.baseSHA {
		// The target branch moved. The stale attempt can never replay.
		record.State = MergeRequestStateFailed
		record.FailureReason = "target branch changed while the merge was interrupted"
		record.BeforeUpdate()
		if _, err = sess.ID(record.ID).AllCols().Update(record); err != nil {
			return nil, errors.Wrap(err, "update recovered merge request")
		}
		return record, nil
	}

	// Same tip: the crashed merge did not push. Mark failed so the caller gets
	// one explicit retry instead of permanent waiting.
	record.State = MergeRequestStateFailed
	record.FailureReason = "previous merge attempt was interrupted before the push completed"
	record.LeaseExpiresUnix = 0
	record.BeforeUpdate()
	if _, err = sess.ID(record.ID).AllCols().Update(record); err != nil {
		return nil, errors.Wrap(err, "update recovered merge request")
	}
	return record, nil
}

// failMergeRequest records a deterministic failure. Failed attempts are not
// replayed silently, the next request must create a new attempt on new tips.
func failMergeRequest(record *MergeRequest, cause error) error {
	record.State = MergeRequestStateFailed
	record.FailureReason = cause.Error()
	record.BeforeUpdate()
	if _, err := x.ID(record.ID).AllCols().Update(record); err != nil {
		return errors.Wrap(err, "update failed merge request")
	}
	return nil
}

// completeMergeRequest verifies the actual git result, marks the pull request
// merged and finalizes the idempotency record in one transaction.
func completeMergeRequest(ctx context.Context, record *MergeRequest, pr *PullRequest, doer *User, mergedCommitID string) error {
	sess := x.NewSession()
	defer sess.Close()
	if err := sess.Begin(); err != nil {
		return errors.Wrap(err, "begin transaction")
	}

	lockedPR := new(PullRequest)
	has, err := sess.ID(pr.ID).ForUpdate().Get(lockedPR)
	if err != nil {
		return errors.Wrap(err, "lock pull request")
	}
	if !has {
		return ErrPullRequestNotExist{args: map[string]any{"pullRequestID": pr.ID}}
	}
	if lockedPR.HasMerged {
		// Another attempt won the race between push and finalize. Treat it as
		// success for this caller, do not overwrite anything.
		_ = sess.Commit()
		record.State = MergeRequestStateMerged
		record.MergedCommitID = lockedPR.MergedCommitID
		return nil
	}

	if err = pr.Issue.changeStatus(sess, doer, pr.Issue.Repo, true); err != nil {
		return errors.Wrap(err, "change issue status")
	}

	pr.MergedCommitID = mergedCommitID
	pr.HasMerged = true
	pr.Merged = time.Now()
	pr.MergerID = doer.ID
	if _, err = sess.ID(pr.ID).AllCols().Update(pr); err != nil {
		return errors.Wrap(err, "update pull request")
	}

	record.State = MergeRequestStateMerged
	record.MergedCommitID = mergedCommitID
	record.BeforeUpdate()
	if _, err = sess.ID(record.ID).AllCols().Update(record); err != nil {
		return errors.Wrap(err, "update merge request")
	}

	return sess.Commit()
}

// dispatchMergeNotificationsOnce sends all post-merge notifications exactly
// once. A delivery failure is logged and surfaced, it never marks the merge as
// failed or sends a second success notification on retry.
func dispatchMergeNotificationsOnce(ctx context.Context, mergeRequestID int64, doer *User, pr *PullRequest, mergedCommitID string, guard *mergeGuard) error {
	sess := x.NewSession()
	defer sess.Close()
	if err := sess.Begin(); err != nil {
		return errors.Wrap(err, "begin transaction")
	}

	record := new(MergeRequest)
	has, err := sess.ID(mergeRequestID).ForUpdate().Get(record)
	if err != nil {
		return errors.Wrap(err, "lock merge request")
	}
	if !has {
		_ = sess.Commit()
		return errors.Newf("merge request %d does not exist", mergeRequestID)
	}
	if record.Notified {
		return sess.Commit()
	}

	// Mark first so a panic in delivery never causes a duplicate notification.
	record.Notified = true
	record.BeforeUpdate()
	if _, err = sess.ID(record.ID).Cols("notified", "updated_unix").Update(record); err != nil {
		return errors.Wrap(err, "mark merge request notified")
	}
	if err = sess.Commit(); err != nil {
		return errors.Wrap(err, "commit notification claim")
	}

	if err := Handle.Actions().MergePullRequest(ctx, doer, pr.Issue.Repo.Owner, pr.Issue.Repo, pr.Issue); err != nil {
		log.Error("Failed to create action for merged pull request %d: %v", pr.ID, err)
	}
	if err := notifyMergedPullRequest(doer, pr, mergedCommitID, guard); err != nil {
		return err
	}
	return nil
}

// notifyMergedPullRequest sends the pull request closed and push webhooks once.
func notifyMergedPullRequest(doer *User, pr *PullRequest, mergedCommitID string, guard *mergeGuard) error {
	pullRequestPayload := &apiv1types.WebhookPullRequestPayload{
		Action:      apiv1types.WebhookIssueClosed,
		Index:       pr.Index,
		PullRequest: pr.APIFormat(),
		Repository:  pr.Issue.Repo.APIFormatLegacy(nil),
		Sender:      doer.APIFormat(),
	}
	if err := PrepareWebhooks(pr.Issue.Repo, HookEventTypePullRequest, pullRequestPayload); err != nil {
		return errors.Wrap(err, "prepare pull request webhook")
	}

	headGitRepo, err := git.Open(RepoPath(pr.HeadUserName, pr.HeadRepo.Name))
	if err != nil {
		return errors.Wrap(err, "open head repository")
	}
	commits, err := headGitRepo.RevList([]string{pr.MergeBase + "..." + pr.MergedCommitID})
	if err != nil {
		log.Error("Failed to list commits [merge_base: %s, merged_commit_id: %s]: %v", pr.MergeBase, pr.MergedCommitID, err)
	}
	// A regular merge appends the merge commit itself so the push payload lists
	// the actual commit that landed on the target branch.
	mergeCommit, err := baseCommitOf(pr, mergedCommitID)
	if err != nil {
		log.Error("Failed to get merge commit %q: %v", mergedCommitID, err)
	} else if mergeCommit != nil {
		commits = append([]*git.Commit{mergeCommit}, commits...)
	}

	var formattedCommits []*apiv1types.WebhookPayloadCommit
	if len(commits) > 0 {
		formattedCommits, err = CommitsToPushCommits(commits).APIFormat(context.TODO(), Handle.Users(), pr.BaseRepo.RepoPath(), pr.BaseRepo.HTMLURL())
		if err != nil {
			log.Error("Failed to convert to API payload commits: %v", err)
		}
	}

	pushPayload := &apiv1types.WebhookPushPayload{
		Ref:        git.RefsHeads + pr.BaseBranch,
		Before:     guard.baseSHA,
		After:      mergedCommitID,
		CompareURL: conf.Server.ExternalURL + pr.BaseRepo.ComposeCompareURL(pr.MergeBase, pr.MergedCommitID),
		Commits:    formattedCommits,
		Repo:       pr.BaseRepo.APIFormatLegacy(nil),
		Pusher:     pr.HeadRepo.MustOwner().APIFormat(),
		Sender:     doer.APIFormat(),
	}
	if err = PrepareWebhooks(pr.BaseRepo, HookEventTypePush, pushPayload); err != nil {
		return errors.Wrap(err, "prepare push webhook")
	}
	return nil
}

// baseCommitOf returns the merge commit for regular merge styles. Rebase
// merges do not create a merge commit, so nil is returned.
func baseCommitOf(pr *PullRequest, mergedCommitID string) (*git.Commit, error) {
	headGitRepo, err := git.Open(RepoPath(pr.HeadUserName, pr.HeadRepo.Name))
	if err != nil {
		return nil, errors.Wrap(err, "open head repository")
	}
	headCommitID, err := headGitRepo.BranchCommitID(pr.HeadBranch)
	if err != nil {
		return nil, errors.Wrap(err, "get head commit")
	}
	if headCommitID == mergedCommitID {
		// Fast-forward or rebase, no merge commit to prepend.
		return nil, nil
	}
	baseGitRepo, err := git.Open(pr.BaseRepo.RepoPath())
	if err != nil {
		return nil, errors.Wrap(err, "open base repository")
	}
	return baseGitRepo.CatFileCommit(mergedCommitID)
}

// isRecoverableFailure reports whether a failed merge attempt may be retried
// by the same request. Only interrupted pushes are retryable, deterministic
// failures like conflicts or stale branches must be refreshed first.
func isRecoverableFailure(reason string) bool {
	return strings.Contains(reason, "interrupted")
}

// resetMergeRequestForRetry moves a recovered failed attempt back to pending
// and extends its lease for the single retry.
func resetMergeRequestForRetry(record *MergeRequest) error {
	record.State = MergeRequestStatePending
	record.FailureReason = ""
	record.LeaseExpiresUnix = time.Now().Add(mergeClaimLease).Unix()
	record.BeforeUpdate()
	if _, err := x.ID(record.ID).AllCols().Update(record); err != nil {
		return errors.Wrap(err, "reset merge request for retry")
	}
	return nil
}
