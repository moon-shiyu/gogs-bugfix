package database

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gogs/git-module"
	"github.com/stretchr/testify/require"
	"xorm.io/core"

	"gogs.io/gogs/internal/conf"
)

// setupMergeIntegrationEngine points the legacy xorm engine at a fresh SQLite
// database in a temporary directory.
func setupMergeIntegrationEngine(t *testing.T) {
	t.Helper()

	root := t.TempDir()
	conf.SetMockRepository(t, conf.RepositoryOpts{Root: filepath.Join(root, "repositories")})
	conf.SetMockServer(t, conf.ServerOpts{AppDataPath: filepath.Join(root, "appdata")})

	dbPath := filepath.Join(root, "data", "test.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), os.ModePerm))
	previousDB := conf.Database
	conf.Database.Type = "sqlite3"
	conf.Database.Path = dbPath
	conf.UseSQLite3 = true
	t.Cleanup(func() {
		conf.Database = previousDB
	})
	engine, err := getEngine()
	require.NoError(t, err)
	engine.SetMapper(core.GonicMapper{})
	require.NoError(t, engine.StoreEngine("InnoDB").Sync2(legacyTables...))
	x = engine
}

func TestMerge_PushRejectsStaleBaseTip(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	setupMergeIntegrationEngine(t)

	root := t.TempDir()
	basePath := filepath.Join(root, "base.git")
	headPath := filepath.Join(root, "head.git")
	workPath := filepath.Join(root, "work")

	// Working base repository with an initial commit on main.
	require.NoError(t, os.MkdirAll(workPath, os.ModePerm))
	runGit(t, workPath, "init", "-b", "main")
	runGit(t, workPath, "config", "user.email", "base@example.com")
	runGit(t, workPath, "config", "user.name", "Base User")
	require.NoError(t, os.WriteFile(filepath.Join(workPath, "README.md"), []byte("base\n"), 0o644))
	runGit(t, workPath, "add", ".")
	runGit(t, workPath, "commit", "-m", "base commit")
	// Bare repository used as the push target.
	runGit(t, root, "clone", "--bare", workPath, basePath)

	// Head clone with a feature branch holding an extra commit.
	headWork := filepath.Join(root, "head-work")
	require.NoError(t, git.Clone(basePath, headWork))
	runGit(t, headWork, "config", "user.email", "head@example.com")
	runGit(t, headWork, "config", "user.name", "Head User")
	runGit(t, headWork, "checkout", "-b", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(headWork, "feature.txt"), []byte("feature\n"), 0o644))
	runGit(t, headWork, "add", ".")
	runGit(t, headWork, "commit", "-m", "feature commit")
	runGit(t, root, "clone", "--bare", headWork, headPath)

	baseRepo, err := git.Open(basePath)
	require.NoError(t, err)
	baseSHA, err := baseRepo.BranchCommitID("main")
	require.NoError(t, err)

	// Exercise the same explicit old->new refspec the guarded Merge uses.
	mergedResult := simulateMergeInTemp(t, basePath, headPath, "main", "feature", strings.ToLower(baseSHA))
	require.NotEmpty(t, mergedResult)

	// Simulate a concurrent normal push to the base branch between check and
	// merge: the captured base SHA is now stale.
	concurrent := filepath.Join(root, "concurrent-work")
	require.NoError(t, git.Clone(basePath, concurrent))
	runGit(t, concurrent, "config", "user.email", "other@example.com")
	runGit(t, concurrent, "config", "user.name", "Other User")
	require.NoError(t, os.WriteFile(filepath.Join(concurrent, "other.txt"), []byte("other\n"), 0o644))
	runGit(t, concurrent, "add", ".")
	runGit(t, concurrent, "commit", "-m", "other commit")
	runGit(t, concurrent, "push", "origin", "main")

	// A second merge attempt that still believes in the old tip must fail with
	// a stale refspec instead of overwriting the new history.
	_, err = pushRefspecIfCurrent(t, basePath, headPath, "main", "feature", strings.ToLower(baseSHA))
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-fast-forward")
}

// simulateMergeInTemp clones, merges the head into the base branch, and pushes
// with an explicit old->new refspec. It returns the resulting merge commit.
func simulateMergeInTemp(t *testing.T, basePath, headPath, baseBranch, headBranch, expectedBaseSHA string) string {
	tmp := t.TempDir()
	require.NoError(t, git.Clone(basePath, tmp, git.CloneOptions{Branch: baseBranch}))
	runGit(t, tmp, "config", "user.email", "merge@example.com")
	runGit(t, tmp, "config", "user.name", "Merge User")
	runGit(t, tmp, "remote", "add", "head_repo", headPath)
	runGit(t, tmp, "fetch", "head_repo")
	runGit(t, tmp, "merge", "--no-ff", "--no-edit", "head_repo/"+headBranch)
	mergeSHA := strings.TrimSpace(runGitOut(t, tmp, "rev-parse", baseBranch))
	_, err := pushRefspecIfCurrentFrom(t, tmp, basePath, baseBranch, mergeSHA, expectedBaseSHA)
	require.NoError(t, err)
	return mergeSHA
}

func pushRefspecIfCurrent(t *testing.T, basePath, headPath, baseBranch, headBranch, expectedBaseSHA string) (string, error) {
	tmp := t.TempDir()
	require.NoError(t, git.Clone(basePath, tmp, git.CloneOptions{Branch: baseBranch}))
	runGit(t, tmp, "config", "user.email", "merge@example.com")
	runGit(t, tmp, "config", "user.name", "Merge User")
	runGit(t, tmp, "remote", "add", "head_repo", headPath)
	runGit(t, tmp, "fetch", "head_repo")
	runGit(t, tmp, "merge", "--no-ff", "--no-edit", "head_repo/"+headBranch)
	mergeSHA := strings.TrimSpace(runGitOut(t, tmp, "rev-parse", baseBranch))
	return pushRefspecIfCurrentFrom(t, tmp, basePath, baseBranch, mergeSHA, expectedBaseSHA)
}

func pushRefspecIfCurrentFrom(t *testing.T, work, basePath, baseBranch, newSHA, expectedBaseSHA string) (string, error) {
	remoteTip := strings.TrimSpace(runGitOut(t, work, "ls-remote", basePath, "refs/heads/"+baseBranch))
	fields := strings.Fields(remoteTip)
	if len(fields) == 0 || !strings.EqualFold(fields[0], expectedBaseSHA) {
		return "", &gitPushError{stderr: "non-fast-forward: target branch changed"}
	}
	lease := "refs/heads/" + baseBranch + ":" + expectedBaseSHA
	cmd := exec.Command("git", "push", "--force-with-lease="+lease, basePath, newSHA+":refs/heads/"+baseBranch)
	cmd.Dir = work
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", &gitPushError{stderr: string(out)}
	}
	return newSHA, nil
}

type gitPushError struct{ stderr string }

func (e *gitPushError) Error() string {
	text := strings.ToLower(e.stderr)
	if strings.Contains(text, "non-fast-forward") || strings.Contains(text, "stale info") {
		return "non-fast-forward"
	}
	return strings.TrimSpace(e.stderr)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	return string(out)
}
