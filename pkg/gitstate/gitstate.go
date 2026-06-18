// Package gitstate answers live git-state queries for managed projects. It opens
// each project's working tree on demand with go-git and reports branch, dirty,
// and unpushed status.
//
// Computing live (rather than reading a cached batch report) is deliberate:
// fleet-svc gates destructive host-migration on this answer, so a stale "clean"
// reading could greenlight a teardown over uncommitted work. The cost of opening
// a handful of small working trees per request is negligible next to that risk.
//
// JSON field names are snake_case and aligned with the forthcoming
// delight.v1.RepoGitState contract, so this surface graduates cleanly to
// Protobuf over Kafka alongside the rest of delightd's events.
package gitstate

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"delightd/config"
)

// RepoGitState is delightd's live view of one project's working tree. A value is
// always Name-populated; on a read failure Error is set and the other fields hold
// zero values, so a caller can render a row without special-casing nil.
type RepoGitState struct {
	Name        string `json:"name"`
	Branch      string `json:"branch"`
	Dirty       bool   `json:"dirty"`
	Unpushed    int    `json:"unpushed"`
	HasUpstream bool   `json:"has_upstream"`
	RemoteURL   string `json:"remote_url"`
	// Error carries a per-repo failure (not a git repo, unreadable HEAD, ...)
	// without failing the whole sweep. Empty when the read succeeded.
	Error string `json:"error,omitempty"`
}

// CollectAll returns the live git state for every configured project, sorted by
// name for stable output. A failure on one repo is reported in that repo's Error
// field; it never aborts the sweep.
func CollectAll(projects []config.ProjectConfig) []RepoGitState {
	out := make([]RepoGitState, 0, len(projects))
	for _, p := range projects {
		out = append(out, Collect(p.Name, p.Path))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Collect opens the working tree at path and computes its git state.
func Collect(name, path string) RepoGitState {
	st := RepoGitState{Name: name}

	repo, err := git.PlainOpen(expandHome(path))
	if err != nil {
		st.Error = err.Error()
		return st
	}

	head, err := repo.Head()
	if err != nil {
		// No commits yet, detached in a broken way, etc. Surface it rather than
		// guessing a branch name.
		st.Error = err.Error()
		return st
	}
	if head.Name().IsBranch() {
		st.Branch = head.Name().Short()
	}

	// Resolve the tracking remote rather than assuming "origin": the fleet's
	// repos are inconsistent (some name the remote "github"), so a hardcoded
	// "origin" would silently report everything as never-pushed.
	remoteName, remoteURL, hasRemote := resolveRemote(repo, st.Branch)
	st.RemoteURL = remoteURL

	wt, err := repo.Worktree()
	if err != nil {
		st.Error = err.Error()
		return st
	}
	status, err := wt.Status()
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Dirty = !status.IsClean()

	st.Unpushed, st.HasUpstream = countUnpushed(repo, head, remoteName, hasRemote)

	return st
}

// resolveRemote picks the remote the given branch tracks. It prefers the
// branch's configured upstream (branch.<name>.remote in git config), then a
// remote literally named "origin", then the sole remote when there is exactly
// one. ok is false when no remote can be determined at all.
func resolveRemote(repo *git.Repository, branch string) (name, url string, ok bool) {
	cfg, err := repo.Config()
	if err != nil {
		return "", "", false
	}

	firstURL := func(r *gogitconfig.RemoteConfig) string {
		if r != nil && len(r.URLs) > 0 {
			return r.URLs[0]
		}
		return ""
	}

	if b, exists := cfg.Branches[branch]; exists && b.Remote != "" {
		return b.Remote, firstURL(cfg.Remotes[b.Remote]), true
	}
	if r, exists := cfg.Remotes["origin"]; exists {
		return "origin", firstURL(r), true
	}
	if len(cfg.Remotes) == 1 {
		for n, r := range cfg.Remotes {
			return n, firstURL(r), true
		}
	}
	return "", "", false
}

// countUnpushed reports how many commits reachable from HEAD are not yet on the
// branch's tracking ref, and whether such a tracking ref exists. No remote, or a
// remote without a ref for this branch, means the branch has never been pushed:
// every reachable commit counts as unpushed work -- the conservative answer for
// a safety gate.
func countUnpushed(repo *git.Repository, head *plumbing.Reference, remoteName string, hasRemote bool) (int, bool) {
	if !hasRemote {
		return countCommitsUntil(repo, head.Hash(), plumbing.ZeroHash), false
	}
	upstreamRef := plumbing.NewRemoteReferenceName(remoteName, head.Name().Short())
	upstream, err := repo.Reference(upstreamRef, true)
	if err != nil {
		return countCommitsUntil(repo, head.Hash(), plumbing.ZeroHash), false
	}
	return countCommitsUntil(repo, head.Hash(), upstream.Hash()), true
}

// countCommitsUntil walks first-ancestor history from `from`, counting commits
// until it reaches `stop`. A ZeroHash stop (no upstream) never matches, so the
// walk counts every reachable commit.
func countCommitsUntil(repo *git.Repository, from, stop plumbing.Hash) int {
	iter, err := repo.Log(&git.LogOptions{From: from})
	if err != nil {
		return 0
	}
	defer iter.Close()

	count := 0
	_ = iter.ForEach(func(c *object.Commit) error {
		if c.Hash == stop {
			return storer.ErrStop
		}
		count++
		return nil
	})
	return count
}

// expandHome resolves a leading ~ to the current user's home directory. Project
// paths in delight.yaml are written with ~ by convention.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}
