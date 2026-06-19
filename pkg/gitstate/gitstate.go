// Package gitstate computes the live git state of a project's working tree.
//
// In delightd's taxonomy (see docs/architecture.md §6) a *project* is the
// atomic managed unit, and GitState is an *observed attribute* of that project's
// working tree -- delightd reports it, it does not own the tree. The wire shape
// reflects that: callers receive a ProjectGit, a project paired with its git
// element, not a free-standing "repo" record.
//
// State is computed live, per request: fleet-svc gates destructive
// host-migration on it, so a stale "clean" reading could greenlight a teardown
// over uncommitted work. The cost of opening a handful of small working trees is
// negligible next to that risk.
//
// This package never logs. Per-project failures are returned in-band via
// GitState.Error; the caller (the httpapi handler) is responsible for emitting
// them. Keeping the computation pure is intentional, and the logging contract
// lives with whoever serves the result.
//
// Field names are snake_case and aligned with the forthcoming
// delight.v1.GitState contract, so the surface graduates to Protobuf over Kafka
// with the daemon's other events.
package gitstate

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"delightd/config"
)

// GitState is the observed git state of a project's working tree. On a read
// failure Error is set and the other fields hold zero values, so a caller can
// render the project without special-casing nil.
type GitState struct {
	Branch      string `json:"branch"`
	Dirty       bool   `json:"dirty"`
	Unpushed    int    `json:"unpushed"`
	HasUpstream bool   `json:"has_upstream"`
	RemoteURL   string `json:"remote_url"`
	// Error carries a per-project failure (not a git checkout, unreadable HEAD,
	// ...) without failing the whole sweep. Empty when the read succeeded.
	Error string `json:"error,omitempty"`
}

// ProjectGit is a project paired with its observed git state -- the unit the
// /git surface returns. Git is an element of the project, per the taxonomy.
type ProjectGit struct {
	Name string   `json:"name"`
	Git  GitState `json:"git"`
}

// perProjectTimeout caps how long a single project's git read may take before
// the sweep gives up on it. A healthy working tree is well under a second; a
// pathological one (huge tree, lock contention) is capped here so it cannot
// starve the whole /git answer.
const perProjectTimeout = 5 * time.Second

// maxConcurrentCollect bounds the fan-out so a large roster does not spawn
// unbounded goroutines or hammer the disk with parallel walks.
const maxConcurrentCollect = 8

// CollectAll returns the git state for every configured project, sorted by name
// for stable output. Projects are read CONCURRENTLY with a per-project deadline:
// a serial sweep made the total cost the sum of every project's read, so one
// slow tree timed out the whole /git endpoint (and fleet's `git status`, which
// fails closed on it). A failure or timeout on one project is reported in that
// project's Git.Error; it never aborts the sweep.
func CollectAll(projects []config.ProjectConfig) []ProjectGit {
	out := make([]ProjectGit, len(projects))
	sem := make(chan struct{}, maxConcurrentCollect)
	var wg sync.WaitGroup
	for i, p := range projects {
		wg.Add(1)
		go func(i int, p config.ProjectConfig) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = ProjectGit{Name: p.Name, Git: collectWithTimeout(p.Path, perProjectTimeout)}
		}(i, p)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// collectWithTimeout runs Collect but bounds its wall time. go-git's calls take
// no context, so we cannot cancel a slow read; instead we stop waiting on it and
// report a timeout, letting the sweep return promptly. The orphaned goroutine
// finishes on its own (the channel is buffered, so its send never blocks).
func collectWithTimeout(path string, timeout time.Duration) GitState {
	ch := make(chan GitState, 1)
	go func() { ch <- Collect(path) }()
	select {
	case st := <-ch:
		return st
	case <-time.After(timeout):
		return GitState{Error: "git state read exceeded " + timeout.String()}
	}
}

// Collect opens the working tree at path and computes its git state.
func Collect(path string) GitState {
	var st GitState

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
	// projects are inconsistent (some name the remote "github"), so a hardcoded
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
