package main

// Domain operations shared by the REST, GraphQL and MCP surfaces: creating
// issues and pull requests, computing a pull's changed files, and
// squash-merging. One implementation, three serializations — that is what
// keeps the surfaces consistent with each other. (Port of entry/domain.ts.)
//
// Empty-string results play the role of TS `undefined` throughout.

import (
	"fmt"
	"sort"
	"sync"
)

// ---------------------------------------------------------------------------
// Ref resolution

// ResolveRefToCommit resolves a branch name, tag name, full sha or HEAD to a
// commit oid ("" when it resolves to nothing).
func ResolveRefToCommit(repo *RepoRec, ref string) string {
	lookup := RepoLookup(repo)
	if ref == "HEAD" {
		return BranchTip(repo, repo.DefaultBranch)
	}
	if branch, ok := repo.Refs["refs/heads/"+ref]; ok && branch != "" {
		return branch
	}
	if tag, ok := repo.Refs["refs/tags/"+ref]; ok {
		return PeelToCommit(lookup, tag)
	}
	if IsSha40(ref) {
		obj := lookup(ref)
		if obj != nil && obj.Type == "commit" {
			return ref
		}
		if obj != nil && obj.Type == "tag" {
			return PeelToCommit(lookup, ref)
		}
	}
	return ""
}

// RefTree resolves a ref name to the tree oid of its tip commit.
func RefTree(repo *RepoRec, refName string) string {
	commit := ResolveRefToCommit(repo, refName)
	if commit == "" {
		return ""
	}
	obj := RepoLookup(repo)(commit)
	if obj == nil {
		return ""
	}
	return ParseCommit(obj.Data).Tree
}

// ---------------------------------------------------------------------------
// Issues

// CreateIssueRecord creates and stores an open issue in repo.
func CreateIssueRecord(repo *RepoRec, title, body string, labels []string) *EntryRec {
	_, nowISO := S.Tick()
	issue := &EntryRec{
		Kind:     "issue",
		ID:       S.AllocEntityID(),
		Number:   repo.NextNumber,
		Title:    title,
		Body:     body,
		State:    "open",
		Labels:   append([]string{}, labels...),
		Comments: []CommentRec{},
	}
	issue.CreatedSeq = S.NextSeq()
	issue.CreatedAt = nowISO
	issue.UpdatedAt = nowISO
	repo.NextNumber++
	if repo.Entries == nil {
		repo.Entries = make(map[int]*EntryRec)
	}
	repo.Entries[issue.Number] = issue
	repo.EntriesVersion++
	return issue
}

// ---------------------------------------------------------------------------
// Pull requests

// CreatePullRecord creates and stores an open pull request. The second
// result is "" on success, else one of "missing-ref", "same-ref",
// "no-commits" or "duplicate".
func CreatePullRecord(repo *RepoRec, title, body, headRef, baseRef string) (*EntryRec, string) {
	headSha := BranchTip(repo, headRef)
	baseSha := BranchTip(repo, baseRef)
	if headSha == "" || baseSha == "" {
		return nil, "missing-ref"
	}
	if headRef == baseRef {
		return nil, "same-ref"
	}
	lookup := RepoLookup(repo)
	base := MergeBase(lookup, baseSha, headSha)
	if base == "" || base == headSha {
		return nil, "no-commits"
	}
	for _, entry := range repo.Entries {
		if entry.Kind == "pull" && entry.State == "open" && entry.HeadRef == headRef && entry.BaseRef == baseRef {
			return nil, "duplicate"
		}
	}
	_, nowISO := S.Tick()
	pull := &EntryRec{
		Kind:            "pull",
		ID:              S.AllocEntityID(),
		Number:          repo.NextNumber,
		Title:           title,
		Body:            body,
		State:           "open",
		HeadRef:         headRef,
		BaseRef:         baseRef,
		HeadShaSnapshot: headSha,
		BaseShaSnapshot: baseSha,
		Merged:          false,
		Comments:        []CommentRec{},
	}
	pull.CreatedSeq = S.NextSeq()
	pull.CreatedAt = nowISO
	pull.UpdatedAt = nowISO
	repo.NextNumber++
	if repo.Entries == nil {
		repo.Entries = make(map[int]*EntryRec)
	}
	repo.Entries[pull.Number] = pull
	repo.EntriesVersion++
	return pull, ""
}

// PullHeadSha is the pull's current head tip, or its creation-time snapshot
// when the head branch is gone.
func PullHeadSha(repo *RepoRec, pull *EntryRec) string {
	if tip := BranchTip(repo, pull.HeadRef); tip != "" {
		return tip
	}
	return pull.HeadShaSnapshot
}

// PullBaseSha is the pull's current base tip, or its creation-time snapshot
// when the base branch is gone.
func PullBaseSha(repo *RepoRec, pull *EntryRec) string {
	if tip := BranchTip(repo, pull.BaseRef); tip != "" {
		return tip
	}
	return pull.BaseShaSnapshot
}

// PullFile is one changed file of a pull request. Sha is meaningful only
// when HasSha is true (TS: sha is null for removed files).
type PullFile struct {
	Filename  string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
	Changes   int
	Sha       string
	HasSha    bool
}

// PullFiles lists the changed files between the pull's merge-base tree and
// its head tree.
func PullFiles(repo *RepoRec, pull *EntryRec) []PullFile {
	lookup := RepoLookup(repo)
	headSha := PullHeadSha(repo, pull)
	baseSha := PullBaseSha(repo, pull)
	base := MergeBase(lookup, baseSha, headSha)
	if base == "" {
		base = baseSha
	}
	baseObj := lookup(base)
	headObj := lookup(headSha)
	if baseObj == nil || headObj == nil {
		return []PullFile{}
	}
	baseFlat := FlattenTree(lookup, ParseCommit(baseObj.Data).Tree)
	headFlat := FlattenTree(lookup, ParseCommit(headObj.Data).Tree)
	nameSet := make(map[string]bool, len(baseFlat)+len(headFlat))
	names := make([]string, 0, len(baseFlat)+len(headFlat))
	for name := range baseFlat {
		if !nameSet[name] {
			nameSet[name] = true
			names = append(names, name)
		}
	}
	for name := range headFlat {
		if !nameSet[name] {
			nameSet[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	files := []PullFile{}
	for _, filename := range names {
		before, hasBefore := baseFlat[filename]
		after, hasAfter := headFlat[filename]
		switch {
		case hasBefore && !hasAfter:
			text := ""
			if blob := lookup(before.Oid); blob != nil {
				text = string(blob.Data)
			}
			_, deletions := LineDiffCounts(text, "")
			files = append(files, PullFile{Filename: filename, Status: "removed", Additions: 0, Deletions: deletions, Changes: deletions})
		case !hasBefore && hasAfter:
			text := ""
			if blob := lookup(after.Oid); blob != nil {
				text = string(blob.Data)
			}
			additions, _ := LineDiffCounts("", text)
			files = append(files, PullFile{Filename: filename, Status: "added", Additions: additions, Deletions: 0, Changes: additions, Sha: after.Oid, HasSha: true})
		case hasBefore && hasAfter && before.Oid != after.Oid:
			oldText, newText := "", ""
			if blob := lookup(before.Oid); blob != nil {
				oldText = string(blob.Data)
			}
			if blob := lookup(after.Oid); blob != nil {
				newText = string(blob.Data)
			}
			additions, deletions := LineDiffCounts(oldText, newText)
			files = append(files, PullFile{Filename: filename, Status: "modified", Additions: additions, Deletions: deletions, Changes: additions + deletions, Sha: after.Oid, HasSha: true})
		}
	}
	return files
}

// ---------------------------------------------------------------------------
// Merging

var mergeableCache = make(map[string]string)
var mergeableCacheMu sync.Mutex

// MergeableOf dry-runs the three-way merge to report mergeability, memoized
// per tip pair: "MERGEABLE", "CONFLICTING" or "UNKNOWN".
func MergeableOf(repo *RepoRec, pull *EntryRec) string {
	if pull.Merged {
		return "MERGEABLE"
	}
	headSha := PullHeadSha(repo, pull)
	baseSha := PullBaseSha(repo, pull)
	key := fmt.Sprintf("%d:%s:%s", repo.ID, baseSha, headSha)
	mergeableCacheMu.Lock()
	cached, hit := mergeableCache[key]
	mergeableCacheMu.Unlock()
	if hit {
		return cached
	}
	lookup := RepoLookup(repo)
	base := MergeBase(lookup, baseSha, headSha)
	if base == "" {
		return "UNKNOWN"
	}
	baseTree := RefTree(repo, base)
	oursTree := RefTree(repo, baseSha)
	theirsTree := RefTree(repo, headSha)
	if baseTree == "" || oursTree == "" || theirsTree == "" {
		return "UNKNOWN"
	}
	// Merge into a scratch overlay so the dry run does not leak objects into
	// the repo store.
	scratch := make(map[string]*GitObject)
	scratchLookup := Lookup(func(oid string) *GitObject {
		if obj, ok := scratch[oid]; ok {
			return obj
		}
		return lookup(oid)
	})
	scratchPut := Put(func(typ string, data []byte) string {
		oid := HashObject(typ, data)
		if _, ok := scratch[oid]; !ok {
			copied := make([]byte, len(data))
			copy(copied, data)
			scratch[oid] = &GitObject{Type: typ, Data: copied}
		}
		return oid
	})
	outcome := MergeTrees(scratchLookup, scratchPut, baseTree, oursTree, theirsTree)
	status := "MERGEABLE"
	if outcome.Conflict {
		status = "CONFLICTING"
	}
	mergeableCacheMu.Lock()
	if len(mergeableCache) > 512 {
		mergeableCache = make(map[string]string)
	}
	mergeableCache[key] = status
	mergeableCacheMu.Unlock()
	return status
}

// MergePullRecord squash-merges a pull request: one commit on the base
// branch carrying the merged tree. On success ok is true and sha is the
// merge commit; on failure conflict reports a merge conflict.
func MergePullRecord(repo *RepoRec, pull *EntryRec) (sha string, ok bool, conflict bool) {
	if pull.Merged && pull.HasMergeSha && pull.MergeCommitSha != "" {
		return pull.MergeCommitSha, true, false
	}
	lookup := RepoLookup(repo)
	put := RepoPut(repo)
	headSha := PullHeadSha(repo, pull)
	baseSha := PullBaseSha(repo, pull)
	base := MergeBase(lookup, baseSha, headSha)
	if base == "" {
		return "", false, true
	}
	baseTree := RefTree(repo, base)
	oursTree := RefTree(repo, baseSha)
	theirsTree := RefTree(repo, headSha)
	if baseTree == "" || oursTree == "" || theirsTree == "" {
		return "", false, true
	}
	outcome := MergeTrees(lookup, put, baseTree, oursTree, theirsTree)
	if outcome.Conflict {
		return "", false, true
	}
	nowSec, _ := S.Tick()
	identity := Identity{Name: ViewerName, Email: ViewerEmail, Time: nowSec}
	message := pull.Title
	if pull.Body != "" {
		message = pull.Title + "\n\n" + pull.Body
	}
	commitData := BuildCommit(ParsedCommit{
		Tree:      outcome.Tree,
		Parents:   []string{baseSha},
		Author:    identity,
		Committer: identity,
		Message:   message,
	})
	mergeSha := put("commit", commitData)
	if repo.Refs == nil {
		repo.Refs = make(map[string]string)
	}
	repo.Refs["refs/heads/"+pull.BaseRef] = mergeSha
	repo.RefsVersion++
	_, stampISO := S.Tick()
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = stampISO
	pull.HasMergedAt = true
	pull.MergeCommitSha = mergeSha
	pull.HasMergeSha = true
	pull.UpdatedAt = stampISO
	repo.EntriesVersion++
	return mergeSha, true, false
}
