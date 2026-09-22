package database

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// StatusCheckState is the result state of a single commit status check.
type StatusCheckState string

const (
	StatusCheckStatePending StatusCheckState = "pending"
	StatusCheckStateSuccess StatusCheckState = "success"
	StatusCheckStateFailure StatusCheckState = "failure"
	StatusCheckStateError   StatusCheckState = "error"
)

// IsSuccess returns true if the state represents a passed check.
func (s StatusCheckState) IsSuccess() bool {
	return s == StatusCheckStateSuccess
}

// IsTerminal returns true if the state can no longer change.
func (s StatusCheckState) IsTerminal() bool {
	return s == StatusCheckStateSuccess || s == StatusCheckStateFailure || s == StatusCheckStateError
}

// IsValidStatusCheckState returns true if the given state is a recognized value.
func IsValidStatusCheckState(state string) bool {
	switch StatusCheckState(state) {
	case StatusCheckStatePending, StatusCheckStateSuccess, StatusCheckStateFailure, StatusCheckStateError:
		return true
	}
	return false
}

// StatusCheck is a named check result scoped to one commit. A check name only
// grants merge access for the exact commit it was reported against, stale
// results on an older head commit never satisfy newer requirements.
type StatusCheck struct {
	ID        int64  `gorm:"primaryKey"`
	RepoID    int64  `gorm:"index:idx_status_check_lookup,unique"`
	CommitSHA string `gorm:"size:40;index:idx_status_check_lookup,unique"`
	// BaseSHA is the target branch tip when the check ran. Checks on stale
	// history never satisfy a merge after the target branch moved.
	BaseSHA string `gorm:"size:40;index:idx_status_check_lookup,unique"`
	Name    string `gorm:"size:255;index:idx_status_check_lookup,unique"`
	State   StatusCheckState
	// RuleVersion is the protection rule version the check was evaluated under.
	// Bumping the rules makes old green results stale until re-checked.
	RuleVersion int64  `gorm:"index:idx_status_check_lookup,unique"`
	TargetURL   string `gorm:"size:2048"`
	Description string `gorm:"type:TEXT"`

	Created     time.Time `xorm:"-" json:"-" gorm:"-"`
	CreatedUnix int64
	Updated     time.Time `xorm:"-" json:"-" gorm:"-"`
	UpdatedUnix int64
}

func (s *StatusCheck) BeforeCreate() {
	s.CreatedUnix = time.Now().Unix()
	s.UpdatedUnix = s.CreatedUnix
}

func (s *StatusCheck) BeforeUpdate() {
	s.UpdatedUnix = time.Now().Unix()
}

// TableName overrides the gorm table name to match the xorm mapping.
func (StatusCheck) TableName() string {
	return "status_check"
}

// StatusChecksStore is the storage layer for commit status checks.
type StatusChecksStore struct {
	db *gorm.DB
}

func newStatusChecksStore(db *gorm.DB) *StatusChecksStore {
	return &StatusChecksStore{db: db}
}

// SetStatusCheckOptions captures where and under which rules a check ran.
type SetStatusCheckOptions struct {
	RepoID      int64
	CommitSHA   string
	BaseSHA     string
	RuleVersion int64
	Name        string
	State       string
	TargetURL   string
	Description string
}

// SetStatusCheck creates or replaces the state of a named check on a commit.
// Checks are bound to the head commit, base tip and rule version, so stale
// results never satisfy a merge on newer history or newer rules.
func (s *StatusChecksStore) SetStatusCheck(ctx context.Context, opts SetStatusCheckOptions) (*StatusCheck, error) {
	if !IsValidStatusCheckState(opts.State) {
		return nil, errors.Newf("invalid status check state %q", opts.State)
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return nil, errors.New("status check name must not be empty")
	}

	check := &StatusCheck{
		RepoID:      opts.RepoID,
		CommitSHA:   strings.ToLower(strings.TrimSpace(opts.CommitSHA)),
		BaseSHA:     strings.ToLower(strings.TrimSpace(opts.BaseSHA)),
		RuleVersion: opts.RuleVersion,
		Name:        name,
		State:       StatusCheckState(opts.State),
		TargetURL:   opts.TargetURL,
		Description: opts.Description,
	}
	check.BeforeCreate()

	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_id"}, {Name: "commit_sha"}, {Name: "base_sha"}, {Name: "rule_version"}, {Name: "name"}},
		DoUpdates: clause.AssignmentColumns([]string{"state", "target_url", "description", "updated_unix"}),
	}).Create(check).Error
	if err != nil {
		return nil, errors.Wrap(err, "upsert status check")
	}
	return check, nil
}

// ListByCommit returns all checks reported against the given commit.
func (s *StatusChecksStore) ListByCommit(ctx context.Context, repoID int64, commitSHA string) ([]*StatusCheck, error) {
	checks := make([]*StatusCheck, 0)
	err := s.db.WithContext(ctx).
		Where("repo_id = ? AND commit_sha = ?", repoID, strings.ToLower(strings.TrimSpace(commitSHA))).
		Order("name ASC").
		Find(&checks).Error
	if err != nil {
		return nil, errors.Wrap(err, "list status checks")
	}
	return checks, nil
}

// LegacyStatusCheck helpers backed by the legacy xorm engine.

// CreateOrUpdateStatusCheck upserts a check for the given commit through the
// legacy engine used by the web handlers.
func CreateOrUpdateStatusCheck(check *StatusCheck) error {
	if check.Name == "" {
		return errors.New("status check name must not be empty")
	}
	check.CommitSHA = strings.ToLower(strings.TrimSpace(check.CommitSHA))
	check.BaseSHA = strings.ToLower(strings.TrimSpace(check.BaseSHA))
	check.BeforeUpdate()

	existing := new(StatusCheck)
	has, err := x.Where(
		"repo_id = ? AND commit_sha = ? AND base_sha = ? AND rule_version = ? AND name = ?",
		check.RepoID, check.CommitSHA, check.BaseSHA, check.RuleVersion, check.Name,
	).Get(existing)
	if err != nil {
		return errors.Wrap(err, "get status check")
	}
	if has {
		check.ID = existing.ID
		check.CreatedUnix = existing.CreatedUnix
		if _, err = x.ID(check.ID).Cols("state", "target_url", "description", "updated_unix").Update(check); err != nil {
			return errors.Wrap(err, "update status check")
		}
		return nil
	}

	check.BeforeCreate()
	if _, err = x.Insert(check); err != nil {
		return errors.Wrap(err, "insert status check")
	}
	return nil
}

// GetStatusChecksByCommit returns checks recorded against the exact commit.
func GetStatusChecksByCommit(repoID int64, commitSHA string) ([]*StatusCheck, error) {
	checks := make([]*StatusCheck, 0)
	err := x.Where("repo_id = ? AND commit_sha = ?", repoID, strings.ToLower(strings.TrimSpace(commitSHA))).
		Asc("name").Find(&checks)
	if err != nil {
		return nil, errors.Wrap(err, "find status checks")
	}
	return checks, nil
}

// MergeRequestState is the lifecycle state of an idempotent merge attempt.
type MergeRequestState string

const (
	// MergeRequestStatePending is claimed but not yet finished. Stuck rows are
	// reclaimed by comparing them against the actual git and pull request state.
	MergeRequestStatePending MergeRequestState = "pending"
	// MergeRequestStateMerged means the target branch was written exactly once.
	MergeRequestStateMerged MergeRequestState = "merged"
	// MergeRequestStateFailed means the attempt finished without writing the
	// target branch, the failure reason is recorded for retries.
	MergeRequestStateFailed MergeRequestState = "failed"
)

// MergeRequest is an idempotency record for a merge attempt. The unique key on
// (pull_request_id, head_sha, base_sha) makes double clicks and retried web or
// notification requests converge to the same outcome instead of merging twice.
type MergeRequest struct {
	ID             int64  `gorm:"primaryKey"`
	PullRequestID  int64  `gorm:"uniqueIndex:idx_merge_request_idempotency"`
	HeadSHA        string `gorm:"size:40;uniqueIndex:idx_merge_request_idempotency"`
	BaseSHA        string `gorm:"size:40;uniqueIndex:idx_merge_request_idempotency"`
	DoerID         int64
	MergeStyle     string `gorm:"size:32"`
	State          MergeRequestState
	MergedCommitID string `gorm:"size:40"`
	FailureReason  string `gorm:"type:TEXT"`
	Notified       bool
	// LeaseExpiresUnix bounds how long a pending claim is considered alive.
	// Expired rows are recovered deterministically against git instead of
	// leaving the page in permanent waiting after a crashed merge.
	LeaseExpiresUnix int64

	Created     time.Time `xorm:"-" json:"-" gorm:"-"`
	CreatedUnix int64
	Updated     time.Time `xorm:"-" json:"-" gorm:"-"`
	UpdatedUnix int64
}

func (m *MergeRequest) BeforeCreate() {
	m.CreatedUnix = time.Now().Unix()
	m.UpdatedUnix = m.CreatedUnix
}

func (m *MergeRequest) BeforeUpdate() {
	m.UpdatedUnix = time.Now().Unix()
}

// TableName overrides the gorm table name to match the xorm mapping.
func (MergeRequest) TableName() string {
	return "merge_request"
}

// ConflictError is returned when a request conflicts with current state.
type ConflictError interface {
	error
	Conflict() bool
}

var _ ConflictError = (*ErrMergeRequestChanged)(nil)

// ErrMergeRequestChanged indicates that a concurrent or retried request can
// not be replayed because the pull request or target branch moved. The caller
// must refresh and restart the checks instead of silently replaying.
type ErrMergeRequestChanged struct {
	args map[string]any
}

func (err ErrMergeRequestChanged) Error() string {
	return fmt.Sprintf("merge request is stale and needs resynchronization: %v", err.args)
}

// Conflict marks the error as an HTTP 409 style conflict for handlers.
func (ErrMergeRequestChanged) Conflict() bool {
	return true
}

// IsErrMergeRequestChanged returns true if the error is a stale merge request.
func IsErrMergeRequestChanged(err error) bool {
	_, ok := err.(ErrMergeRequestChanged)
	return ok
}

var _ ConflictError = (*ErrRequiredChecksMissing)(nil)

// ErrRequiredChecksMissing reports exactly which required checks have not
// passed for the current head commit. It is returned by both web and
// background entry points so callers never see a generic failure.
type ErrRequiredChecksMissing struct {
	// Missing is required check names with no success result on this commit.
	Missing []string
	// Pending is required check names still running on this commit.
	Pending []string
	// Failed is required check names that reported failure or error.
	Failed []string
}

func (err ErrRequiredChecksMissing) Error() string {
	switch {
	case len(err.Missing) > 0:
		return fmt.Sprintf("required status checks have not passed, missing results for %s", strings.Join(err.Missing, ", "))
	case len(err.Pending) > 0:
		return fmt.Sprintf("required status checks are still pending: %s", strings.Join(err.Pending, ", "))
	default:
		return fmt.Sprintf("required status checks failed: %s", strings.Join(err.Failed, ", "))
	}
}

// Conflict marks the error as an HTTP 409 style conflict for handlers.
func (ErrRequiredChecksMissing) Conflict() bool {
	return true
}

// IsErrRequiredChecksMissing returns true if the merge was blocked by checks.
func IsErrRequiredChecksMissing(err error) bool {
	_, ok := err.(ErrRequiredChecksMissing)
	return ok
}

// evaluateRequiredChecks compares required check names against results that
// belong to headSHA only, returning a precise error when not all pass.
func evaluateRequiredChecks(required []string, checks []*StatusCheck) error {
	if len(required) == 0 {
		return nil
	}

	byName := make(map[string]StatusCheckState, len(checks))
	for _, check := range checks {
		byName[check.Name] = check.State
	}

	missingChecks := make([]string, 0)
	pendingChecks := make([]string, 0)
	failedChecks := make([]string, 0)
	for _, name := range required {
		state, ok := byName[name]
		switch {
		case !ok || state == StatusCheckStatePending:
			// Distinguish "never reported" from "reported but still pending"
			// so the page can show waiting rather than failure.
			if ok {
				pendingChecks = append(pendingChecks, name)
			} else {
				missingChecks = append(missingChecks, name)
			}
		case !state.IsSuccess():
			failedChecks = append(failedChecks, name)
		}
	}

	if len(missingChecks) == 0 && len(pendingChecks) == 0 && len(failedChecks) == 0 {
		return nil
	}
	return ErrRequiredChecksMissing{
		Missing: missingChecks,
		Pending: pendingChecks,
		Failed:  failedChecks,
	}
}

// RequiredStatusCheckNames parses the configured required check list. Empty
// entries and duplicates are removed and the result is sorted for stable
// error messages.
func ParseRequiredStatusCheckNames(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := make(map[string]bool)
	names := make([]string, 0)
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return names
}

// RequiredStatusCheckNames returns the sorted list of checks configured on
// the protected branch.
func (b *ProtectBranch) RequiredStatusCheckNames() []string {
	return ParseRequiredStatusCheckNames(b.RequiredStatusChecks)
}

// formatRequiredStatusChecks normalizes the stored value.
func formatRequiredStatusChecks(names []string) string {
	return strings.Join(ParseRequiredStatusCheckNames(strings.Join(names, ",")), ",")
}
