package database

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	log "unknwon.dev/clog/v2"
	"xorm.io/xorm"

	"github.com/gogs/git-module"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/errx"
	"gogs.io/gogs/internal/osx"
	"gogs.io/gogs/internal/process"
	apiv1types "gogs.io/gogs/internal/route/api/v1/types"
	"gogs.io/gogs/internal/sync"
)

var PullRequestQueue = sync.NewUniqueQueue(1000)

type PullRequestType int

const (
	PullRequestTypeGogs PullRequestType = iota
	PullRequestTypeGit
)

type PullRequestStatus int

const (
	PullRequestStatusConflict PullRequestStatus = iota
	PullRequestStatusChecking
	PullRequestStatusMergeable
)

// PullRequest represents relation between pull request and repositories.
type PullRequest struct {
	ID     int64 `gorm:"primaryKey"`
	Type   PullRequestType
	Status PullRequestStatus

	IssueID int64  `xorm:"INDEX" gorm:"index"`
	Issue   *Issue `xorm:"-" json:"-" gorm:"-"`
	Index   int64

	HeadRepoID   int64
	HeadRepo     *Repository `xorm:"-" json:"-" gorm:"-"`
	BaseRepoID   int64
	BaseRepo     *Repository `xorm:"-" json:"-" gorm:"-"`
	HeadUserName string
	HeadBranch   string
	BaseBranch   string
	MergeBase    string `xorm:"VARCHAR(40)" gorm:"type:VARCHAR(40)"`

	HasMerged      bool
	MergedCommitID string `xorm:"VARCHAR(40)" gorm:"type:VARCHAR(40)"`
	MergerID       int64
	Merger         *User     `xorm:"-" json:"-" gorm:"-"`
	Merged         time.Time `xorm:"-" json:"-" gorm:"-"`
	MergedUnix     int64
}

func (pr *PullRequest) BeforeUpdate() {
	pr.MergedUnix = pr.Merged.Unix()
}

// Note: don't try to get Issue because will end up recursive querying.
func (pr *PullRequest) AfterSet(colName string, _ xorm.Cell) {
	switch colName {
	case "merged_unix":
		if !pr.HasMerged {
			return
		}

		pr.Merged = time.Unix(pr.MergedUnix, 0).Local()
	}
}

// Note: don't try to get Issue because will end up recursive querying.
func (pr *PullRequest) loadAttributes(e Engine) (err error) {
	if pr.HeadRepo == nil {
		pr.HeadRepo, err = getRepositoryByID(e, pr.HeadRepoID)
		if err != nil && !IsErrRepoNotExist(err) {
			return errors.Newf("get head repository by ID: %v", err)
		}
	}

	if pr.BaseRepo == nil {
		pr.BaseRepo, err = getRepositoryByID(e, pr.BaseRepoID)
		if err != nil {
			return errors.Newf("get base repository by ID: %v", err)
		}
	}

	if pr.HasMerged && pr.Merger == nil {
		pr.Merger, err = getUserByID(e, pr.MergerID)
		if IsErrUserNotExist(err) {
			pr.MergerID = -1
			pr.Merger = NewGhostUser()
		} else if err != nil {
			return errors.Newf("get merger by ID: %v", err)
		}
	}

	return nil
}

func (pr *PullRequest) LoadAttributes() error {
	return pr.loadAttributes(x)
}

func (pr *PullRequest) LoadIssue() (err error) {
	if pr.Issue != nil {
		return nil
	}

	pr.Issue, err = GetIssueByID(pr.IssueID)
	return err
}

// This method assumes following fields have been assigned with valid values:
// Required - Issue, BaseRepo
// Optional - HeadRepo, Merger
func (pr *PullRequest) APIFormat() *apiv1types.PullRequest {
	// In case of head repo has been deleted.
	var apiHeadRepo *apiv1types.Repository
	if pr.HeadRepo == nil {
		apiHeadRepo = &apiv1types.Repository{
			Name: "deleted",
		}
	} else {
		apiHeadRepo = pr.HeadRepo.APIFormatLegacy(nil)
	}

	apiIssue := pr.Issue.APIFormat()
	apiPullRequest := &apiv1types.PullRequest{
		ID:         pr.ID,
		Index:      pr.Index,
		Poster:     apiIssue.Poster,
		Title:      apiIssue.Title,
		Body:       apiIssue.Body,
		Labels:     apiIssue.Labels,
		Milestone:  apiIssue.Milestone,
		Assignee:   apiIssue.Assignee,
		State:      apiIssue.State,
		Comments:   apiIssue.Comments,
		HeadBranch: pr.HeadBranch,
		HeadRepo:   apiHeadRepo,
		BaseBranch: pr.BaseBranch,
		BaseRepo:   pr.BaseRepo.APIFormatLegacy(nil),
		HTMLURL:    pr.Issue.HTMLURL(),
		HasMerged:  pr.HasMerged,
	}

	if pr.Status != PullRequestStatusChecking {
		mergeable := pr.Status != PullRequestStatusConflict
		apiPullRequest.Mergeable = &mergeable
	}
	if pr.HasMerged {
		apiPullRequest.Merged = &pr.Merged
		apiPullRequest.MergedCommitID = &pr.MergedCommitID
		apiPullRequest.MergedBy = pr.Merger.APIFormat()
	}

	return apiPullRequest
}

// IsChecking returns true if this pull request is still checking conflict.
func (pr *PullRequest) IsChecking() bool {
	return pr.Status == PullRequestStatusChecking
}

// CanAutoMerge returns true if this pull request can be merged automatically.
func (pr *PullRequest) CanAutoMerge() bool {
	return pr.Status == PullRequestStatusMergeable
}

// MergeStyle represents the approach to merge commits into base branch.
type MergeStyle string

const (
	MergeStyleRegular MergeStyle = "create_merge_commit"
	MergeStyleRebase  MergeStyle = "rebase_before_merging"
)

// mergeExecutionOptions controls a guarded merge write.
type mergeExecutionOptions struct {
	// expectedBaseSHA is the target branch tip captured when checks passed.
	// The push is rejected when the remote tip differs, i.e. someone pushed to
	// the target branch while checks were running.
	expectedBaseSHA string
}

// Merge merges pull request to base repository and returns the resulting merge
// commit ID. It performs only the git write, callers must hold an idempotency
// claim from MergePullRequestIdempotent so concurrent requests can not both
// write the target branch.
func (pr *PullRequest) Merge(doer *User, baseGitRepo *git.Repository, mergeStyle MergeStyle, commitDescription string, opts ...mergeExecutionOptions) (string, error) {
	var opt mergeExecutionOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	headRepoPath := RepoPath(pr.HeadUserName, pr.HeadRepo.Name)

	// Create temporary directory to store temporary copy of the base repository,
	// and clean it up when operation finished regardless of succeed or not.
	// Each merge gets a unique directory. Removing only this directory (never
	// the shared parent) is required for concurrent merges not to delete each
	// other's working copy.
	tmpBasePath := filepath.Join(conf.Server.AppDataPath, "tmp", "repos", strconv.FormatInt(time.Now().UnixNano(), 10))
	var (
		stderr string
		err    error
	)
	if err = os.MkdirAll(tmpBasePath, os.ModePerm); err != nil {
		return "", err
	}
	defer func() {
		_ = os.RemoveAll(tmpBasePath)
	}()

	// Clone the base repository to the defined temporary directory,
	// and checks out to base branch directly.
	if err = git.Clone(baseGitRepo.Path(), tmpBasePath, git.CloneOptions{
		Branch:  pr.BaseBranch,
		Timeout: 5 * time.Minute,
	}); err != nil {
		return "", errors.Newf("git clone: %v", err)
	}

	// Add remote which points to the head repository.
	if _, stderr, err = process.ExecDir(-1, tmpBasePath,
		fmt.Sprintf("PullRequest.Merge (git remote add): %s", tmpBasePath),
		"git", "remote", "add", "head_repo", headRepoPath); err != nil {
		return "", errors.Newf("git remote add [%s -> %s]: %s", headRepoPath, tmpBasePath, stderr)
	}

	// Fetch information from head repository to the temporary copy.
	if _, stderr, err = process.ExecDir(-1, tmpBasePath,
		fmt.Sprintf("PullRequest.Merge (git fetch): %s", tmpBasePath),
		"git", "fetch", "head_repo"); err != nil {
		return "", errors.Newf("git fetch [%s -> %s]: %s", headRepoPath, tmpBasePath, stderr)
	}

	remoteHeadBranch := "head_repo/" + pr.HeadBranch

	// Check if merge style is allowed, reset to default style if not
	if mergeStyle == MergeStyleRebase && !pr.BaseRepo.PullsAllowRebase {
		mergeStyle = MergeStyleRegular
	}

	switch mergeStyle {
	case MergeStyleRegular: // Create merge commit

		// Merge changes from head branch.
		if _, stderr, err = process.ExecDir(-1, tmpBasePath,
			fmt.Sprintf("PullRequest.Merge (git merge --no-ff --no-commit): %s", tmpBasePath),
			"git", "merge", "--no-ff", "--no-commit", "--end-of-options", remoteHeadBranch); err != nil {
			return "", errors.Newf("git merge --no-ff --no-commit [%s]: %v - %s", tmpBasePath, err, stderr)
		}

		// Create a merge commit for the base branch.
		if _, stderr, err = process.ExecDir(-1, tmpBasePath,
			fmt.Sprintf("PullRequest.Merge (git merge): %s", tmpBasePath),
			"git", "commit", fmt.Sprintf("--author='%s <%s>'", doer.DisplayName(), doer.Email),
			"-m", fmt.Sprintf("Merge branch '%s' of %s/%s into %s", pr.HeadBranch, pr.HeadUserName, pr.HeadRepo.Name, pr.BaseBranch),
			"-m", commitDescription); err != nil {
			return "", errors.Newf("git commit [%s]: %v - %s", tmpBasePath, err, stderr)
		}

	case MergeStyleRebase: // Rebase before merging

		// Rebase head branch based on base branch, this creates a non-branch commit state.
		if _, stderr, err = process.ExecDir(-1, tmpBasePath,
			fmt.Sprintf("PullRequest.Merge (git rebase): %s", tmpBasePath),
			"git", "rebase", "--quiet", "--end-of-options", pr.BaseBranch, remoteHeadBranch); err != nil {
			return "", errors.Newf("git rebase [%s on %s]: %s", remoteHeadBranch, pr.HeadBranch, stderr)
		}

		// Name non-branch commit state to a new temporary branch in order to save changes.
		tmpBranch := strconv.FormatInt(time.Now().UnixNano(), 10)
		if _, stderr, err = process.ExecDir(-1, tmpBasePath,
			fmt.Sprintf("PullRequest.Merge (git checkout): %s", tmpBasePath),
			"git", "checkout", "-b", tmpBranch); err != nil {
			return "", errors.Newf("git checkout '%s': %s", tmpBranch, stderr)
		}

		// Check out the base branch to be operated on.
		if _, stderr, err = process.ExecDir(-1, tmpBasePath,
			fmt.Sprintf("PullRequest.Merge (git checkout): %s", tmpBasePath),
			"git", "checkout", "--end-of-options", pr.BaseBranch); err != nil {
			return "", errors.Newf("git checkout '%s': %s", pr.BaseBranch, stderr)
		}

		// Merge changes from temporary branch to the base branch.
		if _, stderr, err = process.ExecDir(-1, tmpBasePath,
			fmt.Sprintf("PullRequest.Merge (git merge): %s", tmpBasePath),
			"git", "merge", "--end-of-options", tmpBranch); err != nil {
			return "", errors.Newf("git merge [%s]: %v - %s", tmpBasePath, err, stderr)
		}

	default:
		return "", errors.Newf("unknown merge style: %s", mergeStyle)
	}

	// Read the resulting merge commit before pushing so we can verify the
	// remote tip afterwards.
	mergedCommitID, stderr, err := process.ExecDir(-1, tmpBasePath,
		fmt.Sprintf("PullRequest.Merge (git rev-parse): %s", tmpBasePath),
		"git", "rev-parse", pr.BaseBranch)
	if err != nil {
		return "", errors.Newf("get merge commit [%s]: %v - %s", tmpBasePath, err, stderr)
	}
	mergedCommitID = strings.TrimSpace(mergedCommitID)

	if opt.expectedBaseSHA != "" {
		// Git treats an identical source and destination ref as
		// "Everything up-to-date" and skips the lease check. Compare against
		// the live remote tip first so a retried merge can not be reported as
		// successful while the target branch moved.
		remoteTip, _, tipErr := process.ExecDir(-1, tmpBasePath,
			fmt.Sprintf("PullRequest.Merge (git ls-remote): %s", tmpBasePath),
			"git", "ls-remote", baseGitRepo.Path(), "refs/heads/"+pr.BaseBranch)
		if tipErr != nil {
			return "", errors.Newf("git ls-remote: %v - %s", tipErr, remoteTip)
		}
		fields := strings.Fields(strings.TrimSpace(remoteTip))
		if len(fields) == 0 || !strings.EqualFold(fields[0], opt.expectedBaseSHA) {
			return "", ErrMergeRequestChanged{args: map[string]any{
				"reason":       "target branch changed while checks were running",
				"base_branch":  pr.BaseBranch,
				"expected":     opt.expectedBaseSHA,
				"pull_request": pr.ID,
			}}
		}
	}

	// Push with an atomic compare-and-swap against the base SHA captured when
	// checks passed. Git refuses when the remote tip moved, this covers both
	// non-fast-forward and fast-forward races so two merges can never both
	// write the target branch.
	pushArgs := []string{"push"}
	if opt.expectedBaseSHA != "" {
		lease := fmt.Sprintf("refs/heads/%s:%s", pr.BaseBranch, opt.expectedBaseSHA)
		pushArgs = append(pushArgs, "--force-with-lease="+lease,
			baseGitRepo.Path(),
			fmt.Sprintf("%s:refs/heads/%s", mergedCommitID, pr.BaseBranch))
	} else {
		pushArgs = append(pushArgs, baseGitRepo.Path(), pr.BaseBranch)
	}
	if _, stderr, err = process.ExecDir(-1, tmpBasePath,
		fmt.Sprintf("PullRequest.Merge (git push): %s", tmpBasePath),
		"git", pushArgs...); err != nil {
		lowerStderr := strings.ToLower(stderr)
		if opt.expectedBaseSHA != "" && (strings.Contains(lowerStderr, "non-fast-forward") ||
			strings.Contains(lowerStderr, "stale info") ||
			strings.Contains(lowerStderr, "cannot force update the branch")) {
			return "", ErrMergeRequestChanged{args: map[string]any{
				"reason":       "target branch changed while checks were running",
				"base_branch":  pr.BaseBranch,
				"expected":     opt.expectedBaseSHA,
				"pull_request": pr.ID,
			}}
		}
		return "", errors.Newf("git push: %v - %s", err, stderr)
	}

	// Refresh the conflict-test queue after the target branch changed. The
	// success notifications are dispatched separately and idempotently by the
	// merge request record, so this never enqueues a second success event.
	go AddTestPullRequestTask(doer, pr.BaseRepo.ID, pr.BaseBranch, false)
	return mergedCommitID, nil
}

// testPatch checks if patch can be merged to base repository without conflict.
// FIXME: make a mechanism to clean up stable local copies.
func (pr *PullRequest) testPatch() (err error) {
	if pr.BaseRepo == nil {
		pr.BaseRepo, err = GetRepositoryByID(pr.BaseRepoID)
		if err != nil {
			return errors.Newf("GetRepositoryByID: %v", err)
		}
	}

	patchPath, err := pr.BaseRepo.PatchPath(pr.Index)
	if err != nil {
		return errors.Newf("BaseRepo.PatchPath: %v", err)
	}

	// Fast fail if patch does not exist, this assumes data is corrupted.
	if !osx.IsFile(patchPath) {
		log.Trace("PullRequest[%d].testPatch: ignored corrupted data", pr.ID)
		return nil
	}

	repoWorkingPool.CheckIn(strconv.FormatInt(pr.BaseRepoID, 10))
	defer repoWorkingPool.CheckOut(strconv.FormatInt(pr.BaseRepoID, 10))

	log.Trace("PullRequest[%d].testPatch (patchPath): %s", pr.ID, patchPath)

	if err := pr.BaseRepo.UpdateLocalCopyBranch(pr.BaseBranch); err != nil {
		return errors.Newf("UpdateLocalCopy [%d]: %v", pr.BaseRepoID, err)
	}

	args := []string{"apply", "--check"}
	if pr.BaseRepo.PullsIgnoreWhitespace {
		args = append(args, "--ignore-whitespace")
	}
	args = append(args, patchPath)

	pr.Status = PullRequestStatusChecking
	_, stderr, err := process.ExecDir(-1, pr.BaseRepo.LocalCopyPath(),
		fmt.Sprintf("testPatch (git apply --check): %d", pr.BaseRepo.ID),
		"git", args...)
	if err != nil {
		log.Trace("PullRequest[%d].testPatch (apply): has conflict\n%s", pr.ID, stderr)
		pr.Status = PullRequestStatusConflict
		return nil
	}
	return nil
}

// NewPullRequest creates new pull request with labels for repository.
func NewPullRequest(repo *Repository, pull *Issue, labelIDs []int64, uuids []string, pr *PullRequest, patch []byte) (err error) {
	sess := x.NewSession()
	defer sess.Close()
	if err = sess.Begin(); err != nil {
		return err
	}

	if err = newIssue(sess, NewIssueOptions{
		Repo:        repo,
		Issue:       pull,
		LableIDs:    labelIDs,
		Attachments: uuids,
		IsPull:      true,
	}); err != nil {
		return errors.Newf("newIssue: %v", err)
	}

	pr.Index = pull.Index
	if err = repo.SavePatch(pr.Index, patch); err != nil {
		return errors.Newf("SavePatch: %v", err)
	}

	pr.BaseRepo = repo
	if err = pr.testPatch(); err != nil {
		return errors.Newf("testPatch: %v", err)
	}
	// No conflict appears after test means mergeable.
	if pr.Status == PullRequestStatusChecking {
		pr.Status = PullRequestStatusMergeable
	}

	pr.IssueID = pull.ID
	if _, err = sess.Insert(pr); err != nil {
		return errors.Newf("insert pull repo: %v", err)
	}

	if err = sess.Commit(); err != nil {
		return errors.Newf("commit: %v", err)
	}

	if err = NotifyWatchers(&Action{
		ActUserID:    pull.Poster.ID,
		ActUserName:  pull.Poster.Name,
		OpType:       ActionCreatePullRequest,
		Content:      fmt.Sprintf("%d|%s", pull.Index, pull.Title),
		RepoID:       repo.ID,
		RepoUserName: repo.Owner.Name,
		RepoName:     repo.Name,
		IsPrivate:    repo.IsPrivate,
	}); err != nil {
		log.Error("NotifyWatchers: %v", err)
	}
	if err = pull.MailParticipants(); err != nil {
		log.Error("MailParticipants: %v", err)
	}

	pr.Issue = pull
	pull.PullRequest = pr
	if err = PrepareWebhooks(repo, HookEventTypePullRequest, &apiv1types.WebhookPullRequestPayload{
		Action:      apiv1types.WebhookIssueOpened,
		Index:       pull.Index,
		PullRequest: pr.APIFormat(),
		Repository:  repo.APIFormatLegacy(nil),
		Sender:      pull.Poster.APIFormat(),
	}); err != nil {
		log.Error("PrepareWebhooks: %v", err)
	}

	return nil
}

// GetUnmergedPullRequest returns a pull request that is open and has not been merged
// by given head/base and repo/branch.
func GetUnmergedPullRequest(headRepoID, baseRepoID int64, headBranch, baseBranch string) (*PullRequest, error) {
	pr := new(PullRequest)
	has, err := x.Where("head_repo_id=? AND head_branch=? AND base_repo_id=? AND base_branch=? AND has_merged=? AND issue.is_closed=?",
		headRepoID, headBranch, baseRepoID, baseBranch, false, false).
		Join("INNER", "issue", "issue.id=pull_request.issue_id").Get(pr)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrPullRequestNotExist{args: map[string]any{
			"headRepoID": headRepoID,
			"baseRepoID": baseRepoID,
			"headBranch": headBranch,
			"baseBranch": baseBranch,
		}}
	}

	return pr, nil
}

// GetUnmergedPullRequestsByHeadInfo returns all pull requests that are open and has not been merged
// by given head information (repo and branch).
func GetUnmergedPullRequestsByHeadInfo(repoID int64, branch string) ([]*PullRequest, error) {
	prs := make([]*PullRequest, 0, 2)
	return prs, x.Where("head_repo_id = ? AND head_branch = ? AND has_merged = ? AND issue.is_closed = ?",
		repoID, branch, false, false).
		Join("INNER", "issue", "issue.id = pull_request.issue_id").Find(&prs)
}

// GetUnmergedPullRequestsByBaseInfo returns all pull requests that are open and has not been merged
// by given base information (repo and branch).
func GetUnmergedPullRequestsByBaseInfo(repoID int64, branch string) ([]*PullRequest, error) {
	prs := make([]*PullRequest, 0, 2)
	return prs, x.Where("base_repo_id=? AND base_branch=? AND has_merged=? AND issue.is_closed=?",
		repoID, branch, false, false).
		Join("INNER", "issue", "issue.id=pull_request.issue_id").Find(&prs)
}

var _ errx.NotFound = (*ErrPullRequestNotExist)(nil)

type ErrPullRequestNotExist struct {
	args map[string]any
}

func IsErrPullRequestNotExist(err error) bool {
	_, ok := err.(ErrPullRequestNotExist)
	return ok
}

func (err ErrPullRequestNotExist) Error() string {
	return fmt.Sprintf("pull request does not exist: %v", err.args)
}

func (ErrPullRequestNotExist) NotFound() bool {
	return true
}

func getPullRequestByID(e Engine, id int64) (*PullRequest, error) {
	pr := new(PullRequest)
	has, err := e.ID(id).Get(pr)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrPullRequestNotExist{args: map[string]any{"pullRequestID": id}}
	}
	return pr, pr.loadAttributes(e)
}

// GetPullRequestByID returns a pull request by given ID.
func GetPullRequestByID(id int64) (*PullRequest, error) {
	return getPullRequestByID(x, id)
}

func getPullRequestByIssueID(e Engine, issueID int64) (*PullRequest, error) {
	pr := &PullRequest{
		IssueID: issueID,
	}
	has, err := e.Get(pr)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrPullRequestNotExist{args: map[string]any{"issueID": issueID}}
	}
	return pr, pr.loadAttributes(e)
}

// GetPullRequestByIssueID returns pull request by given issue ID.
func GetPullRequestByIssueID(issueID int64) (*PullRequest, error) {
	return getPullRequestByIssueID(x, issueID)
}

// Update updates all fields of pull request.
func (pr *PullRequest) Update() error {
	_, err := x.Id(pr.ID).AllCols().Update(pr)
	return err
}

// Update updates specific fields of pull request.
func (pr *PullRequest) UpdateCols(cols ...string) error {
	_, err := x.Id(pr.ID).Cols(cols...).Update(pr)
	return err
}

// UpdatePatch generates and saves a new patch.
func (pr *PullRequest) UpdatePatch() (err error) {
	headGitRepo, err := git.Open(pr.HeadRepo.RepoPath())
	if err != nil {
		return errors.Newf("open repository: %v", err)
	}

	// Add a temporary remote.
	tmpRemote := strconv.FormatInt(time.Now().UnixNano(), 10)
	baseRepoPath := RepoPath(pr.BaseRepo.MustOwner().Name, pr.BaseRepo.Name)
	err = headGitRepo.RemoteAdd(tmpRemote, baseRepoPath, git.RemoteAddOptions{Fetch: true})
	if err != nil {
		return errors.Newf("add remote %q [repo_id: %d]: %v", tmpRemote, pr.HeadRepoID, err)
	}
	defer func() {
		if err := headGitRepo.RemoteRemove(tmpRemote); err != nil {
			log.Error("Failed to remove remote %q [repo_id: %d]: %v", tmpRemote, pr.HeadRepoID, err)
		}
	}()

	remoteBranch := "remotes/" + tmpRemote + "/" + pr.BaseBranch
	pr.MergeBase, err = headGitRepo.MergeBase(remoteBranch, pr.HeadBranch)
	if err != nil {
		return errors.Newf("get merge base: %v", err)
	} else if err = pr.Update(); err != nil {
		return errors.Newf("update: %v", err)
	}

	patch, err := headGitRepo.DiffBinary(pr.MergeBase, pr.HeadBranch)
	if err != nil {
		return errors.Newf("get binary patch: %v", err)
	}

	if err = pr.BaseRepo.SavePatch(pr.Index, patch); err != nil {
		return errors.Newf("save patch: %v", err)
	}

	log.Trace("PullRequest[%d].UpdatePatch: patch saved", pr.ID)
	return nil
}

// PushToBaseRepo pushes commits from branches of head repository to
// corresponding branches of base repository.
// FIXME: Only push branches that are actually updates?
func (pr *PullRequest) PushToBaseRepo() (err error) {
	log.Trace("PushToBaseRepo[%d]: pushing commits to base repo 'refs/pull/%d/head'", pr.BaseRepoID, pr.Index)

	headRepoPath := pr.HeadRepo.RepoPath()
	headGitRepo, err := git.Open(headRepoPath)
	if err != nil {
		return errors.Newf("open repository: %v", err)
	}

	tmpRemote := fmt.Sprintf("tmp-pull-%d", pr.ID)
	if err = headGitRepo.RemoteAdd(tmpRemote, pr.BaseRepo.RepoPath()); err != nil {
		return errors.Newf("add remote %q [repo_id: %d]: %v", tmpRemote, pr.HeadRepoID, err)
	}

	// Make sure to remove the remote even if the push fails
	defer func() {
		if err := headGitRepo.RemoteRemove(tmpRemote); err != nil {
			log.Error("Failed to remove remote %q [repo_id: %d]: %v", tmpRemote, pr.HeadRepoID, err)
		}
	}()

	headRefspec := fmt.Sprintf("refs/pull/%d/head", pr.Index)
	headFile := filepath.Join(pr.BaseRepo.RepoPath(), headRefspec)
	if osx.Exist(headFile) {
		err = os.Remove(headFile)
		if err != nil {
			return errors.Newf("remove head file [repo_id: %d]: %v", pr.BaseRepoID, err)
		}
	}

	err = headGitRepo.Push(tmpRemote, fmt.Sprintf("%s:%s", pr.HeadBranch, headRefspec))
	if err != nil {
		return errors.Newf("push: %v", err)
	}

	return nil
}

// AddToTaskQueue adds itself to pull request test task queue.
func (pr *PullRequest) AddToTaskQueue() {
	go PullRequestQueue.AddFunc(pr.ID, func() {
		pr.Status = PullRequestStatusChecking
		if err := pr.UpdateCols("status"); err != nil {
			log.Error("AddToTaskQueue.UpdateCols[%d].(add to queue): %v", pr.ID, err)
		}
	})
}

type PullRequestList []*PullRequest

func (prs PullRequestList) loadAttributes(e Engine) (err error) {
	if len(prs) == 0 {
		return nil
	}

	// Load issues
	set := make(map[int64]*Issue)
	for i := range prs {
		set[prs[i].IssueID] = nil
	}
	issueIDs := make([]int64, 0, len(prs))
	for issueID := range set {
		issueIDs = append(issueIDs, issueID)
	}
	issues := make([]*Issue, 0, len(issueIDs))
	if err = e.Where("id > 0").In("id", issueIDs).Find(&issues); err != nil {
		return errors.Newf("find issues: %v", err)
	}
	for i := range issues {
		set[issues[i].ID] = issues[i]
	}
	for i := range prs {
		prs[i].Issue = set[prs[i].IssueID]
	}

	// Load attributes
	for i := range prs {
		if err = prs[i].loadAttributes(e); err != nil {
			return errors.Newf("loadAttributes [%d]: %v", prs[i].ID, err)
		}
	}

	return nil
}

func (prs PullRequestList) LoadAttributes() error {
	return prs.loadAttributes(x)
}

func addHeadRepoTasks(prs []*PullRequest) {
	for _, pr := range prs {
		if pr.HeadRepo == nil {
			log.Trace("addHeadRepoTasks[%d]: missing head repository", pr.ID)
			continue
		}

		log.Trace("addHeadRepoTasks[%d]: composing new test task", pr.ID)
		if err := pr.UpdatePatch(); err != nil {
			log.Error("UpdatePatch: %v", err)
			continue
		} else if err := pr.PushToBaseRepo(); err != nil {
			log.Error("PushToBaseRepo: %v", err)
			continue
		}

		pr.AddToTaskQueue()
	}
}

// AddTestPullRequestTask adds new test tasks by given head/base repository and head/base branch,
// and generate new patch for testing as needed.
func AddTestPullRequestTask(doer *User, repoID int64, branch string, isSync bool) {
	log.Trace("AddTestPullRequestTask [head_repo_id: %d, head_branch: %s]: finding pull requests", repoID, branch)
	prs, err := GetUnmergedPullRequestsByHeadInfo(repoID, branch)
	if err != nil {
		log.Error("Find pull requests [head_repo_id: %d, head_branch: %s]: %v", repoID, branch, err)
		return
	}

	if isSync {
		if err = PullRequestList(prs).LoadAttributes(); err != nil {
			log.Error("PullRequestList.LoadAttributes: %v", err)
		}

		if err == nil {
			for _, pr := range prs {
				pr.Issue.PullRequest = pr
				if err = pr.Issue.LoadAttributes(); err != nil {
					log.Error("LoadAttributes: %v", err)
					continue
				}
				if err = PrepareWebhooks(pr.Issue.Repo, HookEventTypePullRequest, &apiv1types.WebhookPullRequestPayload{
					Action:      apiv1types.WebhookIssueSynchronized,
					Index:       pr.Issue.Index,
					PullRequest: pr.Issue.PullRequest.APIFormat(),
					Repository:  pr.Issue.Repo.APIFormatLegacy(nil),
					Sender:      doer.APIFormat(),
				}); err != nil {
					log.Error("PrepareWebhooks [pull_id: %v]: %v", pr.ID, err)
					continue
				}
			}
		}
	}

	addHeadRepoTasks(prs)

	log.Trace("AddTestPullRequestTask [base_repo_id: %d, base_branch: %s]: finding pull requests", repoID, branch)
	prs, err = GetUnmergedPullRequestsByBaseInfo(repoID, branch)
	if err != nil {
		log.Error("Find pull requests [base_repo_id: %d, base_branch: %s]: %v", repoID, branch, err)
		return
	}
	for _, pr := range prs {
		pr.AddToTaskQueue()
	}
}

// checkAndUpdateStatus checks if pull request is possible to leaving checking status,
// and set to be either conflict or mergeable.
func (pr *PullRequest) checkAndUpdateStatus() {
	// Status is not changed to conflict means mergeable.
	if pr.Status == PullRequestStatusChecking {
		pr.Status = PullRequestStatusMergeable
	}

	// Make sure there is no waiting test to process before leaving the checking status.
	if !PullRequestQueue.Exist(pr.ID) {
		if err := pr.UpdateCols("status"); err != nil {
			log.Error("Update[%d]: %v", pr.ID, err)
		}
	}
}

// TestPullRequests checks and tests untested patches of pull requests.
// TODO: test more pull requests at same time.
func TestPullRequests() {
	prs := make([]*PullRequest, 0, 10)
	_ = x.Iterate(PullRequest{
		Status: PullRequestStatusChecking,
	},
		func(idx int, bean any) error {
			pr := bean.(*PullRequest)

			if err := pr.LoadAttributes(); err != nil {
				log.Error("LoadAttributes: %v", err)
				return nil
			}

			if err := pr.testPatch(); err != nil {
				log.Error("testPatch: %v", err)
				return nil
			}
			prs = append(prs, pr)
			return nil
		})

	// Update pull request status.
	for _, pr := range prs {
		pr.checkAndUpdateStatus()
	}

	// Start listening on new test requests.
	for prID := range PullRequestQueue.Queue() {
		log.Trace("TestPullRequests[%v]: processing test task", prID)
		PullRequestQueue.Remove(prID)

		id, _ := strconv.ParseInt(prID, 10, 64)
		pr, err := GetPullRequestByID(id)
		if err != nil {
			log.Error("GetPullRequestByID[%s]: %v", prID, err)
			continue
		} else if err = pr.testPatch(); err != nil {
			log.Error("testPatch[%d]: %v", pr.ID, err)
			continue
		}

		pr.checkAndUpdateStatus()
	}
}

func InitTestPullRequests() {
	go TestPullRequests()
}
