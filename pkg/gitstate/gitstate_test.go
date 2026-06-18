package gitstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"delightd/config"
)

// newRepo initializes a git repo in a temp dir with a single committed file and
// returns the repo plus its path. The worktree is clean on return.
func newRepo(t *testing.T) (*git.Repository, string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	commitFile(t, repo, dir, "README.md", "hello")
	return repo, dir
}

// commitFile writes name=content into the worktree and commits it, returning the
// new commit hash.
func commitFile(t *testing.T, repo *git.Repository, dir, name, content string) plumbing.Hash {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := wt.Add(name); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
	sig := &object.Signature{Name: "max toegang", Email: "max.toegang@ftml.net", When: time.Now()}
	h, err := wt.Commit("commit "+name, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatalf("commit %s: %v", name, err)
	}
	return h
}

// setUpstream simulates a pushed state: it ensures an "origin" remote exists in
// config and points refs/remotes/origin/<current-branch> at hash. Both the
// config entry and the ref are present, as they would be after a real push.
func setUpstream(t *testing.T, repo *git.Repository, hash plumbing.Hash) {
	t.Helper()
	if _, err := repo.Remote("origin"); err != nil {
		if _, err := repo.CreateRemote(&gogitconfig.RemoteConfig{
			Name: "origin",
			URLs: []string{"git@github.com:janearc/example.git"},
		}); err != nil {
			t.Fatalf("create origin: %v", err)
		}
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	ref := plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", head.Name().Short()), hash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("set upstream: %v", err)
	}
}

func TestCollect_CleanNoUpstream(t *testing.T) {
	_, dir := newRepo(t)
	st := Collect("clean", dir)

	if st.Error != "" {
		t.Fatalf("unexpected error: %s", st.Error)
	}
	if st.Dirty {
		t.Errorf("freshly committed repo reported dirty")
	}
	if st.Branch != "master" && st.Branch != "main" {
		t.Errorf("unexpected branch %q", st.Branch)
	}
	if st.HasUpstream {
		t.Errorf("repo with no tracking ref reported has_upstream=true")
	}
	// One commit, never pushed -> one unpushed commit.
	if st.Unpushed != 1 {
		t.Errorf("unpushed = %d, want 1", st.Unpushed)
	}
}

func TestCollect_Dirty(t *testing.T) {
	_, dir := newRepo(t)
	// Add an untracked file -> dirty worktree.
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := Collect("dirty", dir)
	if st.Error != "" {
		t.Fatalf("unexpected error: %s", st.Error)
	}
	if !st.Dirty {
		t.Errorf("repo with an untracked file reported clean")
	}
}

func TestCollect_UpstreamUpToDate(t *testing.T) {
	repo, dir := newRepo(t)
	head, _ := repo.Head()
	setUpstream(t, repo, head.Hash())

	st := Collect("synced", dir)
	if !st.HasUpstream {
		t.Errorf("has_upstream = false, want true")
	}
	if st.Unpushed != 0 {
		t.Errorf("unpushed = %d, want 0 (HEAD == upstream)", st.Unpushed)
	}
}

func TestCollect_UpstreamBehind(t *testing.T) {
	repo, dir := newRepo(t)
	head, _ := repo.Head()
	// Upstream is pinned to the first commit; then we make two more locally.
	setUpstream(t, repo, head.Hash())
	commitFile(t, repo, dir, "a.txt", "a")
	commitFile(t, repo, dir, "b.txt", "b")

	st := Collect("ahead", dir)
	if !st.HasUpstream {
		t.Errorf("has_upstream = false, want true")
	}
	if st.Unpushed != 2 {
		t.Errorf("unpushed = %d, want 2", st.Unpushed)
	}
}

func TestCollect_RemoteURL(t *testing.T) {
	repo, dir := newRepo(t)
	_, err := repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{"git@github.com:janearc/example.git"},
	})
	if err != nil {
		t.Fatalf("create remote: %v", err)
	}
	st := Collect("withremote", dir)
	if st.RemoteURL != "git@github.com:janearc/example.git" {
		t.Errorf("remote_url = %q, want the origin url", st.RemoteURL)
	}
}

// TestCollect_NonOriginRemote guards the fleet's real inconsistency: delightd's
// own remote is named "github", not "origin". A hardcoded "origin" lookup would
// regress unpushed/has_upstream to always-false here.
func TestCollect_NonOriginRemote(t *testing.T) {
	repo, dir := newRepo(t)
	if _, err := repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "github",
		URLs: []string{"git@github.com:janearc/delightd.git"},
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	head, _ := repo.Head()
	ref := plumbing.NewHashReference(plumbing.NewRemoteReferenceName("github", head.Name().Short()), head.Hash())
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("set tracking ref: %v", err)
	}

	st := Collect("github-remote", dir)
	if st.RemoteURL != "git@github.com:janearc/delightd.git" {
		t.Errorf("remote_url = %q, want the github-named remote's url", st.RemoteURL)
	}
	if !st.HasUpstream {
		t.Errorf("has_upstream = false; a 'github'-named remote with a tracking ref must resolve")
	}
	if st.Unpushed != 0 {
		t.Errorf("unpushed = %d, want 0 (HEAD == tracking ref)", st.Unpushed)
	}
}

func TestCollect_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	st := Collect("bare", dir)
	if st.Error == "" {
		t.Errorf("expected an error for a non-git directory")
	}
	if st.Name != "bare" {
		t.Errorf("name = %q, want it populated even on error", st.Name)
	}
}

func TestCollectAll_SortedAndIsolated(t *testing.T) {
	_, dirA := newRepo(t)
	dirMissing := filepath.Join(t.TempDir(), "does-not-exist")

	repos := CollectAll([]config.ProjectConfig{
		{Name: "zeta", Path: dirA},
		{Name: "alpha", Path: dirMissing},
	})

	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(repos))
	}
	// Sorted by name: alpha before zeta.
	if repos[0].Name != "alpha" || repos[1].Name != "zeta" {
		t.Errorf("not sorted by name: %q, %q", repos[0].Name, repos[1].Name)
	}
	// The missing repo errored but did not abort the sweep over zeta.
	if repos[0].Error == "" {
		t.Errorf("missing repo should carry an error")
	}
	if repos[1].Error != "" {
		t.Errorf("healthy repo should not error: %s", repos[1].Error)
	}
}
