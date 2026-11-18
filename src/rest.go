package main

// The REST v3 surface: repositories, git refs, commits, contents, labels,
// issues, comments and pull requests, shaped the way GitHub shapes them.
// Every handler answers JSON; errors are {message} objects; another owner's
// repository is a 404, never a 403. (Port of entry/rest.ts.)
//
// Tree walking/building for the contents API is implemented locally over
// Lookup/Put (parseRestTree & friends) with the raw string modes git stores,
// so this file does not depend on tree-helper naming in gitobj.go.

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const restForgeVersion = "1.0.0"

const restJSONContentType = "application/json; charset=utf-8"

func restNotFound(c *Ctx) {
	c.SendJSON(404, NewOrderedObj(
		"message", "Not Found",
		"documentation_url", "https://docs.github.com/rest",
	))
}

func restValidationFailed(c *Ctx, message string) {
	c.SendJSON(422, NewOrderedObj(
		"message", message,
		"documentation_url", "https://docs.github.com/rest",
	))
}

// restPathRepo returns the repository the path names, or nil (having
// answered 404) — foreign owners included.
func restPathRepo(c *Ctx, owner, name string) *RepoRec {
	if owner != ViewerLogin {
		restNotFound(c)
		return nil
	}
	repo := S.GetRepo(owner, name)
	if repo == nil {
		restNotFound(c)
		return nil
	}
	return repo
}

// restBodyObject is TS bodyObject: a malformed body is a 400; a missing or
// non-object body reads as an empty object.
func restBodyObject(c *Ctx) (map[string]interface{}, bool) {
	if c.JSONErr {
		c.SendJSON(400, NewOrderedObj("message", "Problems parsing JSON"))
		return nil, false
	}
	if c.JSON == nil {
		return map[string]interface{}{}, true
	}
	if rec, ok := IsRecord(c.JSON); ok {
		return rec, true
	}
	return map[string]interface{}{}, true
}

// ---------------------------------------------------------------------------
// Small JS-semantics helpers

// jsString renders s as a JSON string literal the way JSON.stringify does:
// quotes, backslashes and the usual control escapes, other C0 controls as
// \u00xx, everything else literal.
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// restJSParseInt mirrors Number.parseInt(s, 10): leading whitespace and an
// optional sign, then a run of digits; ok is false when there are no digits
// (NaN). Magnitudes saturate far beyond any realistic page number.
func restJSParseInt(s string) (int, bool) {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsSpace(r) {
			break
		}
		i += size
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	var val int64
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		if val < 1<<40 {
			val = val*10 + int64(s[i]-'0')
		}
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		val = -val
	}
	return int(val), true
}

// ---------------------------------------------------------------------------
// Serializers

func viewerJSONObj() *OrderedObj {
	return NewOrderedObj("login", ViewerLogin, "id", ViewerID, "type", "User")
}

func repoJSON(repo *RepoRec, origin string) *OrderedObj {
	openCount := 0
	for _, e := range repo.Entries {
		if e.State == "open" {
			openCount++
		}
	}
	hostPart := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	return NewOrderedObj(
		"id", repo.ID,
		"node_id", GlobalID("repo", repo.ID),
		"name", repo.Name,
		"full_name", repo.Owner+"/"+repo.Name,
		"private", repo.IsPrivate,
		"owner", NewOrderedObj("login", repo.Owner, "id", ViewerID, "type", "User"),
		"description", repo.Description,
		"fork", false,
		"default_branch", repo.DefaultBranch,
		"created_at", repo.CreatedAt,
		"updated_at", repo.UpdatedAt,
		"pushed_at", repo.UpdatedAt,
		"html_url", origin+"/"+repo.Owner+"/"+repo.Name,
		"clone_url", origin+"/"+repo.Owner+"/"+repo.Name+".git",
		"git_url", "git://"+hostPart+"/"+repo.Owner+"/"+repo.Name+".git",
		"size", 0,
		"stargazers_count", 0,
		"watchers_count", 0,
		"forks_count", 0,
		"open_issues_count", openCount,
		"visibility", map[bool]string{true: "private", false: "public"}[repo.IsPrivate],
		"permissions", NewOrderedObj("admin", true, "maintain", true, "push", true, "triage", true, "pull", true),
	)
}

// RepoJSON is repoJSON for the GraphQL/MCP surfaces.
func RepoJSON(repo *RepoRec, origin string) *OrderedObj { return repoJSON(repo, origin) }

func labelJSON(repo *RepoRec, name string) *OrderedObj {
	color := "ededed"
	description := ""
	if label, ok := repo.Labels[name]; ok && label != nil {
		color = label.Color
		description = label.Description
	}
	return NewOrderedObj(
		"id", 0,
		"node_id", "",
		"name", name,
		"color", color,
		"description", description,
		"default", false,
	)
}

// LabelJSON is labelJSON for the other surfaces.
func LabelJSON(repo *RepoRec, name string) *OrderedObj { return labelJSON(repo, name) }

func issueJSON(repo *RepoRec, issue *EntryRec, origin string) *OrderedObj {
	labels := make([]interface{}, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		labels = append(labels, labelJSON(repo, name))
	}
	var closedAt interface{}
	if issue.State == "closed" {
		closedAt = issue.UpdatedAt
	}
	return NewOrderedObj(
		"id", issue.ID,
		"node_id", GlobalID("issue", issue.ID),
		"number", issue.Number,
		"title", issue.Title,
		"body", issue.Body,
		"state", issue.State,
		"labels", labels,
		"user", viewerJSONObj(),
		"author", viewerJSONObj(),
		"comments", len(issue.Comments),
		"created_at", issue.CreatedAt,
		"updated_at", issue.UpdatedAt,
		"closed_at", closedAt,
		"html_url", fmt.Sprintf("%s/%s/%s/issues/%d", origin, repo.Owner, repo.Name, issue.Number),
		"repository_url", fmt.Sprintf("%s/api/v3/repos/%s/%s", origin, repo.Owner, repo.Name),
	)
}

// IssueJSON is issueJSON for the GraphQL/MCP surfaces.
func IssueJSON(repo *RepoRec, issue *EntryRec, origin string) *OrderedObj {
	return issueJSON(repo, issue, origin)
}

var restViewerJSONString = fmt.Sprintf(`{"login":%s,"id":%d,"type":"User"}`, jsString(ViewerLogin), ViewerID)

func labelJSONString(repo *RepoRec, name string) string {
	color := "ededed"
	description := ""
	if label, ok := repo.Labels[name]; ok && label != nil {
		color = label.Color
		description = label.Description
	}
	return fmt.Sprintf(`{"id":0,"node_id":"","name":%s,"color":%s,"description":%s,"default":false}`,
		jsString(name), jsString(color), jsString(description))
}

// issueJSONString renders issueJSON directly to its JSON text —
// byte-identical, without the object round-trip.
func issueJSONString(repo *RepoRec, issue *EntryRec, origin string) string {
	var labels strings.Builder
	for i, name := range issue.Labels {
		if i > 0 {
			labels.WriteByte(',')
		}
		labels.WriteString(labelJSONString(repo, name))
	}
	closedAt := "null"
	if issue.State == "closed" {
		closedAt = jsString(issue.UpdatedAt)
	}
	return fmt.Sprintf(`{"id":%d,"node_id":%s,"number":%d,"title":%s,"body":%s,"state":%s,"labels":[%s],"user":%s,"author":%s,"comments":%d,"created_at":%s,"updated_at":%s,"closed_at":%s,"html_url":%s,"repository_url":%s}`,
		issue.ID, jsString(GlobalID("issue", issue.ID)), issue.Number,
		jsString(issue.Title), jsString(issue.Body), jsString(issue.State),
		labels.String(), restViewerJSONString, restViewerJSONString,
		len(issue.Comments), jsString(issue.CreatedAt), jsString(issue.UpdatedAt), closedAt,
		jsString(fmt.Sprintf("%s/%s/%s/issues/%d", origin, repo.Owner, repo.Name, issue.Number)),
		jsString(fmt.Sprintf("%s/api/v3/repos/%s/%s", origin, repo.Owner, repo.Name)))
}

// IssueJSONString is issueJSONString for the other surfaces.
func IssueJSONString(repo *RepoRec, issue *EntryRec, origin string) string {
	return issueJSONString(repo, issue, origin)
}

func pullJSON(repo *RepoRec, pull *EntryRec, origin string) *OrderedObj {
	headSha := PullHeadSha(repo, pull)
	baseSha := PullBaseSha(repo, pull)
	var mergedAt interface{}
	if pull.HasMergedAt {
		mergedAt = pull.MergedAt
	}
	var mergeCommitSha interface{}
	if pull.HasMergeSha {
		mergeCommitSha = pull.MergeCommitSha
	}
	var closedAt interface{}
	if pull.State == "closed" {
		closedAt = pull.UpdatedAt
	}
	side := func(ref, sha string) *OrderedObj {
		return NewOrderedObj(
			"ref", ref,
			"sha", sha,
			"label", repo.Owner+":"+ref,
			"user", viewerJSONObj(),
			"repo", repoJSON(repo, origin),
		)
	}
	return NewOrderedObj(
		"id", pull.ID,
		"node_id", GlobalID("pull", pull.ID),
		"number", pull.Number,
		"title", pull.Title,
		"body", pull.Body,
		"state", pull.State,
		"merged", pull.Merged,
		"merged_at", mergedAt,
		"merge_commit_sha", mergeCommitSha,
		"draft", false,
		"user", viewerJSONObj(),
		"author", viewerJSONObj(),
		"head", side(pull.HeadRef, headSha),
		"base", side(pull.BaseRef, baseSha),
		"comments", len(pull.Comments),
		"created_at", pull.CreatedAt,
		"updated_at", pull.UpdatedAt,
		"closed_at", closedAt,
		"html_url", fmt.Sprintf("%s/%s/%s/pull/%d", origin, repo.Owner, repo.Name, pull.Number),
	)
}

// PullJSON is pullJSON for the GraphQL/MCP surfaces.
func PullJSON(repo *RepoRec, pull *EntryRec, origin string) *OrderedObj {
	return pullJSON(repo, pull, origin)
}

func commentJSON(comment CommentRec) *OrderedObj {
	return NewOrderedObj(
		"id", comment.ID,
		"node_id", GlobalID("comment", comment.ID),
		"body", comment.Body,
		"user", NewOrderedObj("login", comment.AuthorLogin, "id", ViewerID, "type", "User"),
		"author", NewOrderedObj("login", comment.AuthorLogin, "id", ViewerID, "type", "User"),
		"created_at", comment.CreatedAt,
		"updated_at", comment.CreatedAt,
	)
}

// CommentJSON is commentJSON for the other surfaces.
func CommentJSON(comment CommentRec) *OrderedObj { return commentJSON(comment) }

func restISOFromSec(sec int64) string {
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

func commitJSON(repo *RepoRec, oid string) *OrderedObj {
	obj := RepoLookup(repo)(oid)
	if obj == nil {
		return NewOrderedObj("sha", oid)
	}
	commit := ParseCommit(obj.Data)
	parents := make([]interface{}, 0, len(commit.Parents))
	for _, parent := range commit.Parents {
		parents = append(parents, NewOrderedObj("sha", parent))
	}
	return NewOrderedObj(
		"sha", oid,
		"node_id", GlobalID("commit", 0)+oid,
		"commit", NewOrderedObj(
			"author", NewOrderedObj("name", commit.Author.Name, "email", commit.Author.Email, "date", restISOFromSec(commit.Author.Time)),
			"committer", NewOrderedObj("name", commit.Committer.Name, "email", commit.Committer.Email, "date", restISOFromSec(commit.Committer.Time)),
			"message", commit.Message,
			"tree", NewOrderedObj("sha", commit.Tree),
			"comment_count", 0,
		),
		"author", viewerJSONObj(),
		"committer", viewerJSONObj(),
		"parents", parents,
	)
}

// CommitJSON is commitJSON for the other surfaces.
func CommitJSON(repo *RepoRec, oid string) *OrderedObj { return commitJSON(repo, oid) }

// ---------------------------------------------------------------------------
// Pagination

// orderedQueryPairs keeps the request's raw query pairs in order so Link
// headers can be built the way URLSearchParams builds them: set() replaces
// the first occurrence in place (dropping later duplicates) or appends.
type orderedQueryPair struct {
	decodedKey string
	raw        string
}

func restQueryPairs(c *Ctx) []orderedQueryPair {
	rawQuery := ""
	if c.Req != nil && c.Req.URL != nil {
		rawQuery = c.Req.URL.RawQuery
	}
	if rawQuery == "" {
		// No raw query available (synthetic context): fall back to the
		// decoded values, re-encoded.
		keys := make([]string, 0, len(c.Query))
		for k := range c.Query {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := []orderedQueryPair{}
		for _, k := range keys {
			for _, v := range c.Query[k] {
				pairs = append(pairs, orderedQueryPair{decodedKey: k, raw: restFormEncode(k) + "=" + restFormEncode(v)})
			}
		}
		return pairs
	}
	pairs := []orderedQueryPair{}
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		key := part
		if i := strings.IndexByte(part, '='); i >= 0 {
			key = part[:i]
		}
		decoded := key
		if d, err := decodeQueryComponent(key); err == nil {
			decoded = d
		}
		pairs = append(pairs, orderedQueryPair{decodedKey: decoded, raw: part})
	}
	return pairs
}

func decodeQueryComponent(s string) (string, error) {
	return url.QueryUnescape(s)
}

// restFormEncode is application/x-www-form-urlencoded encoding as
// URLSearchParams applies it (space as '+').
func restFormEncode(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '*' || r == '-' || r == '.' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('+')
		default:
			for _, by := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", by)
			}
		}
	}
	return b.String()
}

func setQueryPair(pairs []orderedQueryPair, key, value string) []orderedQueryPair {
	out := make([]orderedQueryPair, 0, len(pairs)+1)
	found := false
	for _, p := range pairs {
		if p.decodedKey == key {
			if !found {
				out = append(out, orderedQueryPair{decodedKey: key, raw: restFormEncode(key) + "=" + restFormEncode(value)})
				found = true
			}
			continue
		}
		out = append(out, p)
	}
	if !found {
		out = append(out, orderedQueryPair{decodedKey: key, raw: restFormEncode(key) + "=" + restFormEncode(value)})
	}
	return out
}

func joinQueryPairs(pairs []orderedQueryPair) string {
	raws := make([]string, len(pairs))
	for i, p := range pairs {
		raws[i] = p.raw
	}
	return strings.Join(raws, "&")
}

// restPaginate slices all for the requested page and builds the Link header
// exactly as rest.ts's paginate does.
func restPaginate[T any](c *Ctx, all []T, basePath string) ([]T, map[string]string) {
	perPage := 30
	if raw, present := queryFirst(c.Query, "per_page"); present {
		if n, ok := restJSParseInt(raw); ok {
			perPage = n
			if perPage < 1 {
				perPage = 1
			}
			if perPage > 100 {
				perPage = 100
			}
		}
	} else if n, ok := restJSParseInt("30"); ok {
		perPage = n
	}
	page := 1
	if raw, present := queryFirst(c.Query, "page"); present {
		if n, ok := restJSParseInt(raw); ok && n >= 1 {
			page = n
		}
	}
	start := (page - 1) * perPage
	var items []T
	if start >= len(all) {
		items = []T{}
	} else {
		end := start + perPage
		if end > len(all) {
			end = len(all)
		}
		items = all[start:end]
	}
	headers := map[string]string{}
	if start+perPage < len(all) {
		lastPage := (len(all) + perPage - 1) / perPage
		basePairs := restQueryPairs(c)
		linkFor := func(p int) string {
			pairs := setQueryPair(basePairs, "per_page", strconv.Itoa(perPage))
			pairs = setQueryPair(pairs, "page", strconv.Itoa(p))
			return "<" + c.Origin + basePath + "?" + joinQueryPairs(pairs) + ">"
		}
		links := []string{linkFor(page+1) + `; rel="next"`, linkFor(lastPage) + `; rel="last"`}
		if page > 1 {
			links = append(links, linkFor(1)+`; rel="first"`, linkFor(page-1)+`; rel="prev"`)
		}
		headers["Link"] = strings.Join(links, ", ")
	}
	return items, headers
}

// ---------------------------------------------------------------------------
// Repository CRUD and meta

func handleRoot(c *Ctx) {
	c.SendJSONH(200, NewOrderedObj(
		"current_user_url", c.Origin+"/api/v3/user",
		"repository_url", c.Origin+"/api/v3/repos/{owner}/{repo}",
		"issue_search_url", c.Origin+"/api/v3/search/issues?q={query}",
		"emojis_url", c.Origin+"/api/v3/emojis",
	), map[string]string{"X-OAuth-Scopes": "repo, read:org"})
}

func handleMeta(c *Ctx) {
	c.SendJSON(200, NewOrderedObj(
		"installed_version", restForgeVersion,
		"hooks", []interface{}{},
		"git", []interface{}{},
		"github_developer", false,
		"verifiable_password_authentication", false,
		"ssh_key_fingerprints", NewOrderedObj(),
		"importer", []interface{}{},
		"custom_properties", []interface{}{},
	))
}

func handleUser(c *Ctx) {
	now := S.NowISO()
	c.SendJSON(200, NewOrderedObj(
		"login", ViewerLogin,
		"id", ViewerID,
		"node_id", GlobalID("user", ViewerID),
		"name", ViewerName,
		"email", ViewerEmail,
		"type", "User",
		"site_admin", false,
		"created_at", now,
		"updated_at", now,
	))
}

var restRepoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func handleCreateRepo(c *Ctx) {
	body, ok := restBodyObject(c)
	if !ok {
		return
	}
	name, hasName := StrField(body, "name")
	if !hasName || name == "" || !restRepoNameRe.MatchString(name) {
		restValidationFailed(c, "Repository creation failed.")
		return
	}
	if S.GetRepo(ViewerLogin, name) != nil {
		c.SendJSON(422, NewOrderedObj(
			"message", "Repository creation failed: name already exists on this account",
			"errors", []interface{}{NewOrderedObj(
				"resource", "Repository",
				"code", "custom",
				"field", "name",
				"message", "name already exists on this account",
			)},
		))
		return
	}
	description, _ := StrField(body, "description")
	isPrivate, _ := BoolField(body, "private")
	repo := S.CreateRepo(ViewerLogin, name, description, isPrivate)
	c.SendJSON(201, repoJSON(repo, c.Origin))
}

func handleListUserRepos(c *Ctx) {
	repos := S.ListRepos(ViewerLogin)
	page, headers := restPaginate(c, repos, "/api/v3/user/repos")
	out := make([]interface{}, 0, len(page))
	for _, repo := range page {
		out = append(out, repoJSON(repo, c.Origin))
	}
	c.SendJSONH(200, out, headers)
}

func handleGetRepo(c *Ctx, owner, name string) {
	repo := restPathRepo(c, owner, name)
	if repo == nil {
		return
	}
	c.SendJSON(200, repoJSON(repo, c.Origin))
}

func handlePatchRepo(c *Ctx, owner, name string) {
	repo := restPathRepo(c, owner, name)
	if repo == nil {
		return
	}
	body, ok := restBodyObject(c)
	if !ok {
		return
	}
	if description, present := StrField(body, "description"); present {
		repo.Description = description
	}
	if isPrivate, present := BoolField(body, "private"); present {
		repo.IsPrivate = isPrivate
	}
	if newName, present := StrField(body, "name"); present && newName != "" && newName != repo.Name &&
		restRepoNameRe.MatchString(newName) && S.GetRepo(repo.Owner, newName) == nil {
		// Mirror TS: re-key the store's repo map under the new name.
		delete(S.Repos, repo.Owner+"/"+repo.Name)
		repo.Name = newName
		S.Repos[repo.Owner+"/"+repo.Name] = repo
	}
	_, iso := S.Tick()
	repo.UpdatedAt = iso
	repo.EntriesVersion++
	c.SendJSON(200, repoJSON(repo, c.Origin))
}

func handleDeleteRepo(c *Ctx, owner, name string) {
	repo := restPathRepo(c, owner, name)
	if repo == nil {
		return
	}
	S.DeleteRepo(repo)
	c.Send(204, restJSONContentType, []byte{})
}

// ---------------------------------------------------------------------------
// Branches, refs, commits

func handleListBranches(c *Ctx, repo *RepoRec) {
	branches := make([]interface{}, 0)
	names := make([]string, 0, len(repo.Refs))
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, ref[len("refs/heads/"):])
		}
	}
	sort.Strings(names)
	for _, name := range names {
		oid := repo.Refs["refs/heads/"+name]
		branches = append(branches, NewOrderedObj(
			"name", name,
			"commit", NewOrderedObj(
				"sha", oid,
				"url", fmt.Sprintf("%s/api/v3/repos/%s/%s/commits/%s", c.Origin, repo.Owner, repo.Name, oid),
			),
			"protected", false,
		))
	}
	c.SendJSON(200, branches)
}

func handleGetBranch(c *Ctx, repo *RepoRec, branch string) {
	tip := BranchTip(repo, branch)
	if tip == "" {
		restNotFound(c)
		return
	}
	c.SendJSON(200, NewOrderedObj(
		"name", branch,
		"commit", commitJSON(repo, tip),
		"protected", false,
		"_links", NewOrderedObj(),
	))
}

func handleGetRef(c *Ctx, repo *RepoRec, refPath string) {
	full := refPath
	if !strings.HasPrefix(refPath, "refs/") {
		full = "refs/" + refPath
	}
	oid, ok := repo.Refs[full]
	if !ok {
		restNotFound(c)
		return
	}
	typ := "commit"
	if obj := RepoLookup(repo)(oid); obj != nil {
		typ = obj.Type
	}
	c.SendJSON(200, NewOrderedObj(
		"ref", full,
		"node_id", "",
		"object", NewOrderedObj("sha", oid, "type", typ),
	))
}

func handleCreateRef(c *Ctx, repo *RepoRec) {
	body, ok := restBodyObject(c)
	if !ok {
		return
	}
	ref, _ := StrField(body, "ref")
	sha, _ := StrField(body, "sha")
	if ref == "" || sha == "" || !strings.HasPrefix(ref, "refs/") {
		restValidationFailed(c, "Reference creation failed.")
		return
	}
	if _, exists := repo.Refs[ref]; exists {
		c.SendJSON(422, NewOrderedObj("message", "Reference already exists"))
		return
	}
	obj := RepoLookup(repo)(sha)
	if obj == nil {
		restValidationFailed(c, "Object does not exist.")
		return
	}
	if repo.Refs == nil {
		repo.Refs = make(map[string]string)
	}
	repo.Refs[ref] = sha
	repo.RefsVersion++
	repo.EntriesVersion++
	c.SendJSON(201, NewOrderedObj(
		"ref", ref,
		"node_id", "",
		"object", NewOrderedObj("sha", sha, "type", obj.Type),
	))
}

func handleListCommits(c *Ctx, repo *RepoRec) {
	shaParam := repo.DefaultBranch
	if v, present := queryFirst(c.Query, "sha"); present {
		shaParam = v
	}
	tip := ResolveRefToCommit(repo, shaParam)
	if tip == "" {
		c.SendJSON(200, []interface{}{})
		return
	}
	lookup := RepoLookup(repo)
	order := []string{}
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		order = append(order, oid)
		parents := ParseCommit(obj.Data).Parents
		// Push parents in reverse so the first parent is visited first.
		for i := len(parents) - 1; i >= 0; i-- {
			stack = append(stack, parents[i])
		}
	}
	page, headers := restPaginate(c, order, fmt.Sprintf("/api/v3/repos/%s/%s/commits", repo.Owner, repo.Name))
	out := make([]interface{}, 0, len(page))
	for _, oid := range page {
		out = append(out, commitJSON(repo, oid))
	}
	c.SendJSONH(200, out, headers)
}

// ---------------------------------------------------------------------------
// Contents: local tree helpers (raw git tree bytes, string modes)

type restTreeEntry struct {
	Mode string // as stored: "100644", "100755", "120000", "40000", ...
	Name string
	Oid  string
}

type restFlatEntry struct {
	Mode string
	Oid  string
}

type restPathFound struct {
	Kind string // "blob" | "tree"
	Oid  string
	Mode string
}

func parseRestTree(data []byte) []restTreeEntry {
	entries := []restTreeEntry{}
	offset := 0
	for offset < len(data) {
		space := indexByteFrom(data, ' ', offset)
		if space < 0 {
			break
		}
		mode := string(data[offset:space])
		nul := indexByteFrom(data, 0, space)
		if nul < 0 || nul+21 > len(data) {
			break
		}
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, restTreeEntry{Mode: mode, Name: name, Oid: oid})
		offset = nul + 21
	}
	return entries
}

func indexByteFrom(b []byte, c byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func compareRestTreeEntries(a, b restTreeEntry) int {
	an := a.Name
	if a.Mode == "40000" {
		an += "/"
	}
	bn := b.Name
	if b.Mode == "40000" {
		bn += "/"
	}
	return strings.Compare(an, bn)
}

func buildRestTree(entries []restTreeEntry, put Put) string {
	sorted := append([]restTreeEntry{}, entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return compareRestTreeEntries(sorted[i], sorted[j]) < 0 })
	var buf []byte
	for _, entry := range sorted {
		buf = append(buf, entry.Mode...)
		buf = append(buf, ' ')
		buf = append(buf, entry.Name...)
		buf = append(buf, 0)
		raw, err := hex.DecodeString(entry.Oid)
		if err != nil || len(raw) != 20 {
			raw = make([]byte, 20)
		}
		buf = append(buf, raw...)
	}
	return put("tree", buf)
}

func listRestTreeLevel(l Lookup, treeOid string) []restTreeEntry {
	obj := l(treeOid)
	if obj == nil || obj.Type != "tree" {
		return []restTreeEntry{}
	}
	return parseRestTree(obj.Data)
}

func flattenRestTree(l Lookup, treeOid string) map[string]restFlatEntry {
	out := make(map[string]restFlatEntry)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := l(oid)
		if obj == nil || obj.Type != "tree" {
			return
		}
		for _, entry := range parseRestTree(obj.Data) {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.Oid, path)
			} else {
				out[path] = restFlatEntry{Mode: entry.Mode, Oid: entry.Oid}
			}
		}
	}
	if treeOid != "" {
		walk(treeOid, "")
	}
	return out
}

func lookupRestPath(l Lookup, treeOid, path string) *restPathFound {
	rawParts := strings.Split(path, "/")
	parts := make([]string, 0, len(rawParts))
	for _, p := range rawParts {
		if p != "" {
			parts = append(parts, p)
		}
	}
	current := treeOid
	for i, part := range parts {
		obj := l(current)
		if obj == nil || obj.Type != "tree" {
			return nil
		}
		var entry *restTreeEntry
		for _, candidate := range parseRestTree(obj.Data) {
			if candidate.Name == part {
				c := candidate
				entry = &c
				break
			}
		}
		if entry == nil {
			return nil
		}
		if i == len(parts)-1 {
			kind := "blob"
			if entry.Mode == "40000" {
				kind = "tree"
			}
			return &restPathFound{Kind: kind, Oid: entry.Oid, Mode: entry.Mode}
		}
		if entry.Mode != "40000" {
			return nil
		}
		current = entry.Oid
	}
	return nil
}

func buildRestTreeFromFlat(flat map[string]restFlatEntry, put Put) string {
	type dir struct {
		files map[string]restFlatEntry
		dirs  map[string]*dir
	}
	newDir := func() *dir { return &dir{files: map[string]restFlatEntry{}, dirs: map[string]*dir{}} }
	root := newDir()
	for path, file := range flat {
		parts := strings.Split(path, "/")
		d := root
		for _, part := range parts[:len(parts)-1] {
			next, ok := d.dirs[part]
			if !ok {
				next = newDir()
				d.dirs[part] = next
			}
			d = next
		}
		d.files[parts[len(parts)-1]] = file
	}
	var emit func(d *dir) string
	emit = func(d *dir) string {
		entries := []restTreeEntry{}
		for name, file := range d.files {
			entries = append(entries, restTreeEntry{Mode: file.Mode, Name: name, Oid: file.Oid})
		}
		for name, sub := range d.dirs {
			entries = append(entries, restTreeEntry{Mode: "40000", Name: name, Oid: emit(sub)})
		}
		return buildRestTree(entries, put)
	}
	return emit(root)
}

func treeOfTip(repo *RepoRec, tip string) string {
	if tip == "" {
		return ""
	}
	obj := RepoLookup(repo)(tip)
	if obj == nil {
		return ""
	}
	return ParseCommit(obj.Data).Tree
}

func encodeBase64Wrapped(data []byte) string {
	raw := base64.StdEncoding.EncodeToString(data)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(raw); i += 60 {
		end := i + 60
		if end > len(raw) {
			end = len(raw)
		}
		b.WriteString(raw[i:end])
		b.WriteByte('\n')
	}
	return b.String()
}

// restDecodeBase64 mirrors Buffer.from(s, 'base64') after whitespace
// stripping: characters outside the alphabet are ignored and decoding never
// fails, so the handlers' validation branch is unreachable, as in TS.
func restDecodeBase64(content string) []byte {
	stripped := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, content)
	var filtered []byte
	for i := 0; i < len(stripped); i++ {
		ch := stripped[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '+' || ch == '/' || ch == '=' {
			filtered = append(filtered, ch)
		}
	}
	for len(filtered)%4 != 0 {
		filtered = append(filtered, '=')
	}
	data, _ := base64.StdEncoding.DecodeString(string(filtered))
	return data
}

func handleGetContents(c *Ctx, repo *RepoRec, path string) {
	ref := repo.DefaultBranch
	if v, present := queryFirst(c.Query, "ref"); present {
		ref = v
	}
	tip := ResolveRefToCommit(repo, ref)
	if tip == "" && len(repo.Refs) > 0 {
		restNotFound(c)
		return
	}
	tree := treeOfTip(repo, tip)
	lookup := RepoLookup(repo)
	if path == "" {
		entries := listRestTreeLevel(lookup, tree)
		out := make([]interface{}, 0, len(entries))
		for _, entry := range entries {
			out = append(out, treeEntryJSON(repo, entry, ""))
		}
		c.SendJSON(200, out)
		return
	}
	found := lookupRestPath(lookup, tree, path)
	if found == nil {
		restNotFound(c)
		return
	}
	if found.Kind == "tree" {
		prefix := strings.TrimRight(path, "/")
		entries := listRestTreeLevel(lookup, found.Oid)
		out := make([]interface{}, 0, len(entries))
		for _, entry := range entries {
			out = append(out, treeEntryJSON(repo, entry, prefix))
		}
		c.SendJSON(200, out)
		return
	}
	var data []byte
	if blob := lookup(found.Oid); blob != nil {
		data = blob.Data
	}
	name := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		name = path[i+1:]
	}
	c.SendJSON(200, NewOrderedObj(
		"type", "file",
		"encoding", "base64",
		"content", encodeBase64Wrapped(data),
		"sha", found.Oid,
		"size", len(data),
		"name", name,
		"path", path,
		"download_url", nil,
		"html_url", fmt.Sprintf("%s/%s/%s/blob/%s/%s", c.Origin, repo.Owner, repo.Name, ref, path),
		"git_url", fmt.Sprintf("%s/api/v3/repos/%s/%s/git/blobs/%s", c.Origin, repo.Owner, repo.Name, found.Oid),
		"_links", NewOrderedObj(),
	))
}

func treeEntryJSON(repo *RepoRec, entry restTreeEntry, prefix string) *OrderedObj {
	path := entry.Name
	if prefix != "" {
		path = prefix + "/" + entry.Name
	}
	isDir := entry.Mode == "40000"
	size := 0
	if !isDir {
		if blob := RepoLookup(repo)(entry.Oid); blob != nil {
			size = len(blob.Data)
		}
	}
	typ := "file"
	if isDir {
		typ = "dir"
	}
	return NewOrderedObj(
		"type", typ,
		"name", entry.Name,
		"path", path,
		"sha", entry.Oid,
		"size", size,
		"url", repo.Owner+"/"+repo.Name,
		"html_url", "",
		"git_url", "",
		"download_url", nil,
		"_links", NewOrderedObj(),
	)
}

type restContentsWriteResult struct {
	status    int
	commitSha string
	blobOid   string
	created   bool
}

// writeContents is shared by PUT and DELETE contents: rebuild the branch
// tree with one path changed/removed. A nil newData deletes the path.
func writeContents(repo *RepoRec, branch, path string, newData []byte, newMode, message string) restContentsWriteResult {
	lookup := RepoLookup(repo)
	put := RepoPut(repo)
	tip := BranchTip(repo, branch)
	if _, hasBranch := repo.Refs["refs/heads/"+branch]; !hasBranch {
		// Creating a branch implicitly: fork from the default branch tip
		// (or stay unborn).
		tip = BranchTip(repo, repo.DefaultBranch)
	}
	flat := flattenRestTree(lookup, treeOfTip(repo, tip))
	_, existed := flat[path]
	created := !existed
	if newData == nil {
		delete(flat, path)
	} else {
		flat[path] = restFlatEntry{Mode: newMode, Oid: put("blob", newData)}
	}
	rootTree := buildRestTreeFromFlat(flat, put)
	nowSec, _ := S.Tick()
	identity := Identity{Name: ViewerName, Email: ViewerEmail, Time: nowSec}
	parents := []string{}
	if tip != "" {
		parents = []string{tip}
	}
	commitData := BuildCommit(ParsedCommit{
		Tree:      rootTree,
		Parents:   parents,
		Author:    identity,
		Committer: identity,
		Message:   message,
	})
	commitSha := put("commit", commitData)
	if repo.Refs == nil {
		repo.Refs = make(map[string]string)
	}
	repo.Refs["refs/heads/"+branch] = commitSha
	repo.RefsVersion++
	_, iso := S.Tick()
	repo.UpdatedAt = iso
	blobOid := ""
	if newData != nil {
		blobOid = flat[path].Oid
	}
	status := 200
	if created {
		status = 201
	}
	return restContentsWriteResult{status: status, commitSha: commitSha, blobOid: blobOid, created: created}
}

func handlePutContents(c *Ctx, repo *RepoRec, path string) {
	body, ok := restBodyObject(c)
	if !ok {
		return
	}
	message, hasMessage := StrField(body, "message")
	content, hasContent := StrField(body, "content")
	if !hasMessage || !hasContent {
		restValidationFailed(c, "Validation Failed: message and content are required.")
		return
	}
	branch, _ := StrField(body, "branch")
	if branch == "" {
		// TS: strField(body, 'branch') ?? repo.defaultBranch — an empty
		// string is a string, but a branch named "" cannot exist; the
		// fallback below resolves through the default branch either way.
		if _, present := body["branch"]; !present {
			branch = repo.DefaultBranch
		}
	}
	tip := BranchTip(repo, branch)
	if tip == "" {
		tip = BranchTip(repo, repo.DefaultBranch)
	}
	existing := lookupRestPath(RepoLookup(repo), treeOfTip(repo, tip), path)
	givenSha, hasGivenSha := StrField(body, "sha")
	if existing != nil && existing.Kind == "blob" {
		if givenSha != existing.Oid {
			c.SendJSON(409, NewOrderedObj("message", "Conflict: the given sha does not match the current file sha"))
			return
		}
	} else if hasGivenSha && existing == nil {
		c.SendJSON(422, NewOrderedObj("message", "Validation Failed: sha supplied for a file that does not exist"))
		return
	}
	data := restDecodeBase64(content)
	mode := "100644"
	if existing != nil && existing.Kind == "blob" {
		mode = existing.Mode
	}
	result := writeContents(repo, branch, path, data, mode, message)
	name := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		name = path[i+1:]
	}
	c.SendJSON(result.status, NewOrderedObj(
		"content", NewOrderedObj(
			"type", "file",
			"encoding", "base64",
			"sha", result.blobOid,
			"size", len(data),
			"name", name,
			"path", path,
			"download_url", nil,
			"html_url", "",
			"git_url", "",
			"_links", NewOrderedObj(),
		),
		"commit", commitJSON(repo, result.commitSha),
	))
}

func handleDeleteContents(c *Ctx, repo *RepoRec, path string) {
	body, ok := restBodyObject(c)
	if !ok {
		return
	}
	message, hasMessage := StrField(body, "message")
	sha, hasSha := StrField(body, "sha")
	if !hasMessage || !hasSha {
		restValidationFailed(c, "Validation Failed: message and sha are required.")
		return
	}
	branch := repo.DefaultBranch
	if v, present := StrField(body, "branch"); present {
		branch = v
	}
	tip := BranchTip(repo, branch)
	existing := lookupRestPath(RepoLookup(repo), treeOfTip(repo, tip), path)
	if existing == nil || existing.Kind != "blob" {
		restNotFound(c)
		return
	}
	if existing.Oid != sha {
		c.SendJSON(409, NewOrderedObj("message", "Conflict: the given sha does not match the current file sha"))
		return
	}
	result := writeContents(repo, branch, path, nil, "100644", message)
	c.SendJSON(200, NewOrderedObj(
		"content", nil,
		"commit", commitJSON(repo, result.commitSha),
	))
}

// ---------------------------------------------------------------------------
// Labels

func handleCreateLabel(c *Ctx, repo *RepoRec) {
	body, ok := restBodyObject(c)
	if !ok {
		return
	}
	name, _ := StrField(body, "name")
	color, _ := StrField(body, "color")
	if name == "" || color == "" {
		restValidationFailed(c, "Validation Failed: name and color are required.")
		return
	}
	if _, exists := repo.Labels[name]; exists {
		c.SendJSON(422, NewOrderedObj("message", fmt.Sprintf("Validation Failed: label '%s' already exists", name)))
		return
	}
	description, _ := StrField(body, "description")
	if repo.Labels == nil {
		repo.Labels = make(map[string]*LabelRec)
	}
	repo.Labels[name] = &LabelRec{Name: name, Color: color, Description: description}
	repo.LabelsVersion++
	c.SendJSON(201, labelJSON(repo, name))
}

func handleListLabels(c *Ctx, repo *RepoRec) {
	// TS iterates the labels Map in insertion order; the Go store keeps a
	// plain map, so sort for a deterministic order.
	names := make([]string, 0, len(repo.Labels))
	for name := range repo.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]interface{}, 0, len(names))
	for _, name := range names {
		out = append(out, labelJSON(repo, name))
	}
	c.SendJSON(200, out)
}

// ---------------------------------------------------------------------------
// Issues and comments

// labelNamesOf extracts label names from a request body: strings pass
// through, objects contribute their name. ok is false when the field is not
// an array or an element is neither (TS: returns undefined).
func labelNamesOf(body map[string]interface{}) (names []string, ok bool) {
	raw, present := body["labels"]
	if !present {
		return nil, false
	}
	arr, isArr := raw.([]interface{})
	if !isArr {
		return nil, false
	}
	names = []string{}
	for _, item := range arr {
		if s, isStr := item.(string); isStr {
			names = append(names, s)
		} else if rec, isRec := IsRecord(item); isRec {
			if n, hasName := StrField(rec, "name"); hasName {
				names = append(names, n)
			} else {
				return nil, false
			}
		} else {
			return nil, false
		}
	}
	return names, true
}

func handleCreateIssue(c *Ctx, repo *RepoRec) {
	body, ok := restBodyObject(c)
	if !ok {
		return
	}
	title, hasTitle := StrField(body, "title")
	if !hasTitle {
		restValidationFailed(c, "Validation Failed: title is required.")
		return
	}
	labels, _ := labelNamesOf(body)
	if labels == nil {
		labels = []string{}
	}
	for _, name := range labels {
		if _, exists := repo.Labels[name]; !exists {
			c.SendJSON(422, NewOrderedObj("message", fmt.Sprintf("Validation Failed: label '%s' does not exist", name)))
			return
		}
	}
	text, _ := StrField(body, "body")
	issue := CreateIssueRecord(repo, title, text, labels)
	c.Send(201, restJSONContentType, []byte(issueJSONString(repo, issue, c.Origin)+"\n"))
}

// restSortedEntries returns the repo's entries of one kind in creation
// order (TS relies on Map insertion order; CreatedSeq reproduces it).
func restSortedEntries(repo *RepoRec, kind string) []*EntryRec {
	out := []*EntryRec{}
	for _, entry := range repo.Entries {
		if entry.Kind == kind {
			out = append(out, entry)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

func restFilterState(entries []*EntryRec, state string) []*EntryRec {
	if state != "open" && state != "closed" {
		return entries
	}
	out := []*EntryRec{}
	for _, e := range entries {
		if e.State == state {
			out = append(out, e)
		}
	}
	return out
}

func restSortByDirection(entries []*EntryRec, direction string) {
	sort.SliceStable(entries, func(i, j int) bool {
		if direction == "asc" {
			return entries[i].CreatedSeq < entries[j].CreatedSeq
		}
		return entries[i].CreatedSeq > entries[j].CreatedSeq
	})
}

func handleListIssues(c *Ctx, repo *RepoRec) {
	state := "open"
	if v, present := queryFirst(c.Query, "state"); present {
		state = v
	}
	issues := restFilterState(restSortedEntries(repo, "issue"), state)
	direction := "desc"
	if v, present := queryFirst(c.Query, "direction"); present {
		direction = v
	}
	restSortByDirection(issues, direction)
	page, headers := restPaginate(c, issues, fmt.Sprintf("/api/v3/repos/%s/%s/issues", repo.Owner, repo.Name))
	out := make([]interface{}, 0, len(page))
	for _, issue := range page {
		out = append(out, issueJSON(repo, issue, c.Origin))
	}
	c.SendJSONH(200, out, headers)
}

func restEntryOf(repo *RepoRec, number int) *EntryRec {
	return repo.Entries[number]
}

// Serialized GET-issue bodies, memoized: the read benchmark asks for the
// same issue thousands of times. Entries are keyed by (repo ID, number,
// EntriesVersion) — EntriesVersion is bumped on every entry mutation (see
// domain.go and the mutating handlers here) — and a hit must also match a
// fingerprint built from every other field the body derives from (repo
// UpdatedAt, label definitions, pull head/base tips), so any mutation that
// does not bump the version still misses the cache. A hit is a map lookup
// plus a cached-bytes write.
type restIssueCacheKey struct {
	repoID         int
	number         int
	entriesVersion int
	origin         string
}

type restIssueCacheEntry struct {
	fp   string
	body []byte
}

var restIssueCache = make(map[restIssueCacheKey]restIssueCacheEntry)
var restIssueCacheMu sync.Mutex

func sendCachedIssueBody(c *Ctx, repo *RepoRec, entry *EntryRec, fp string, build func() string) {
	key := restIssueCacheKey{repoID: repo.ID, number: entry.Number, entriesVersion: repo.EntriesVersion, origin: c.Origin}
	restIssueCacheMu.Lock()
	if hit, ok := restIssueCache[key]; ok && hit.fp == fp {
		body := hit.body
		restIssueCacheMu.Unlock()
		c.Send(200, restJSONContentType, body)
		return
	}
	restIssueCacheMu.Unlock()
	body := []byte(build() + "\n")
	restIssueCacheMu.Lock()
	if len(restIssueCache) >= 4096 {
		restIssueCache = make(map[restIssueCacheKey]restIssueCacheEntry)
	}
	restIssueCache[key] = restIssueCacheEntry{fp: fp, body: body}
	restIssueCacheMu.Unlock()
	c.Send(200, restJSONContentType, body)
}

func handleGetIssue(c *Ctx, repo *RepoRec, number int) {
	entry := restEntryOf(repo, number)
	if entry == nil {
		restNotFound(c)
		return
	}
	if entry.Kind == "pull" {
		mergedAt := ""
		if entry.HasMergedAt {
			mergedAt = entry.MergedAt
		}
		mergeSha := ""
		if entry.HasMergeSha {
			mergeSha = entry.MergeCommitSha
		}
		fp := strings.Join([]string{
			"p", repo.UpdatedAt, entry.UpdatedAt, strconv.Itoa(len(entry.Comments)), strconv.Itoa(repo.LabelsVersion),
			entry.State, entry.Title, entry.Body, strconv.FormatBool(entry.Merged), mergedAt,
			mergeSha, PullHeadSha(repo, entry), PullBaseSha(repo, entry),
		}, "|")
		sendCachedIssueBody(c, repo, entry, fp, func() string {
			pullObj := pullJSON(repo, entry, c.Origin)
			pullObj.Set("pull_request", NewOrderedObj(
				"url", fmt.Sprintf("%s/api/v3/repos/%s/%s/pulls/%d", c.Origin, repo.Owner, repo.Name, number),
			))
			// GitHub also exposes pulls through the issues endpoint with
			// the pull fields merged in: shell keys first, pull keys
			// override in place, new pull keys append (spread semantics).
			merged := issueShellOfPull(repo, entry, c.Origin)
			for i, key := range pullObj.keys {
				merged.Set(key, pullObj.vals[i])
			}
			return string(encodeJSONValue(merged))
		})
		return
	}
	fp := strings.Join([]string{
		"i", repo.UpdatedAt, entry.UpdatedAt, strconv.Itoa(len(entry.Comments)), strconv.Itoa(repo.LabelsVersion),
		entry.State, entry.Title, entry.Body, strings.Join(entry.Labels, ","),
	}, "|")
	sendCachedIssueBody(c, repo, entry, fp, func() string {
		return issueJSONString(repo, entry, c.Origin)
	})
}

func issueShellOfPull(repo *RepoRec, pull *EntryRec, origin string) *OrderedObj {
	return NewOrderedObj(
		"id", pull.ID,
		"number", pull.Number,
		"title", pull.Title,
		"body", pull.Body,
		"state", pull.State,
		"labels", []interface{}{},
		"user", viewerJSONObj(),
		"comments", len(pull.Comments),
		"created_at", pull.CreatedAt,
		"updated_at", pull.UpdatedAt,
		"html_url", fmt.Sprintf("%s/%s/%s/pull/%d", origin, repo.Owner, repo.Name, pull.Number),
	)
}

func handlePatchIssue(c *Ctx, repo *RepoRec, number int) {
	entry := restEntryOf(repo, number)
	if entry == nil {
		restNotFound(c)
		return
	}
	body, ok := restBodyObject(c)
	if !ok {
		return
	}
	if state, present := StrField(body, "state"); present && (state == "open" || state == "closed") {
		entry.State = state
	}
	if title, present := StrField(body, "title"); present {
		entry.Title = title
	}
	if text, present := StrField(body, "body"); present {
		entry.Body = text
	}
	if entry.Kind == "issue" {
		if labels, hasLabels := labelNamesOf(body); hasLabels {
			for _, name := range labels {
				if _, exists := repo.Labels[name]; !exists {
					c.SendJSON(422, NewOrderedObj("message", fmt.Sprintf("Validation Failed: label '%s' does not exist", name)))
					return
				}
			}
			entry.Labels = labels
		}
	}
	_, iso := S.Tick()
	entry.UpdatedAt = iso
	repo.EntriesVersion++
	if entry.Kind == "pull" {
		c.SendJSON(200, pullJSON(repo, entry, c.Origin))
	} else {
		c.Send(200, restJSONContentType, []byte(issueJSONString(repo, entry, c.Origin)+"\n"))
	}
}

func handleCreateComment(c *Ctx, repo *RepoRec, number int) {
	entry := restEntryOf(repo, number)
	if entry == nil {
		restNotFound(c)
		return
	}
	body, ok := restBodyObject(c)
	if !ok {
		return
	}
	text, hasText := StrField(body, "body")
	if !hasText {
		restValidationFailed(c, "Validation Failed: body is required.")
		return
	}
	comment := CommentRec{
		ID:          S.AllocEntityID(),
		Body:        text,
		AuthorLogin: ViewerLogin,
	}
	comment.CreatedSeq = S.NextSeq()
	_, iso := S.Tick()
	comment.CreatedAt = iso
	entry.Comments = append(entry.Comments, comment)
	repo.EntriesVersion++
	c.SendJSON(201, commentJSON(comment))
}

func handleListComments(c *Ctx, repo *RepoRec, number int) {
	entry := restEntryOf(repo, number)
	if entry == nil {
		restNotFound(c)
		return
	}
	comments := append([]CommentRec{}, entry.Comments...)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedSeq < comments[j].CreatedSeq })
	page, headers := restPaginate(c, comments, fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d/comments", repo.Owner, repo.Name, number))
	out := make([]interface{}, 0, len(page))
	for _, comment := range page {
		out = append(out, commentJSON(comment))
	}
	c.SendJSONH(200, out, headers)
}

// ---------------------------------------------------------------------------
// Pull requests

func handleCreatePull(c *Ctx, repo *RepoRec) {
	body, ok := restBodyObject(c)
	if !ok {
		return
	}
	title, hasTitle := StrField(body, "title")
	headRaw, hasHead := StrField(body, "head")
	base, hasBase := StrField(body, "base")
	if !hasTitle || !hasHead || !hasBase {
		restValidationFailed(c, "Validation Failed: title, head and base are required.")
		return
	}
	head := headRaw
	if i := strings.Index(headRaw, ":"); i >= 0 {
		head = headRaw[i+1:]
	}
	text, _ := StrField(body, "body")
	pull, errKind := CreatePullRecord(repo, title, text, head, base)
	if errKind != "" {
		if errKind == "duplicate" {
			c.SendJSON(422, NewOrderedObj("message", "Validation Failed: A pull request already exists for this branch combination."))
		} else {
			restValidationFailed(c, "Validation Failed: "+errKind)
		}
		return
	}
	c.SendJSON(201, pullJSON(repo, pull, c.Origin))
}

func handleListPulls(c *Ctx, repo *RepoRec) {
	state := "open"
	if v, present := queryFirst(c.Query, "state"); present {
		state = v
	}
	pulls := restFilterState(restSortedEntries(repo, "pull"), state)
	direction := "desc"
	if v, present := queryFirst(c.Query, "direction"); present {
		direction = v
	}
	restSortByDirection(pulls, direction)
	page, headers := restPaginate(c, pulls, fmt.Sprintf("/api/v3/repos/%s/%s/pulls", repo.Owner, repo.Name))
	out := make([]interface{}, 0, len(page))
	for _, pull := range page {
		out = append(out, pullJSON(repo, pull, c.Origin))
	}
	c.SendJSONH(200, out, headers)
}

func restPullOf(c *Ctx, repo *RepoRec, number int) *EntryRec {
	entry := restEntryOf(repo, number)
	if entry == nil || entry.Kind != "pull" {
		restNotFound(c)
		return nil
	}
	return entry
}

func handleGetPull(c *Ctx, repo *RepoRec, number int) {
	pull := restPullOf(c, repo, number)
	if pull == nil {
		return
	}
	c.SendJSON(200, pullJSON(repo, pull, c.Origin))
}

func handlePullFiles(c *Ctx, repo *RepoRec, number int) {
	pull := restPullOf(c, repo, number)
	if pull == nil {
		return
	}
	files := PullFiles(repo, pull)
	objs := make([]*OrderedObj, 0, len(files))
	for _, file := range files {
		var sha interface{}
		if file.HasSha {
			sha = file.Sha
		}
		objs = append(objs, NewOrderedObj(
			"sha", sha,
			"filename", file.Filename,
			"status", file.Status,
			"additions", file.Additions,
			"deletions", file.Deletions,
			"changes", file.Changes,
			"blob_url", "",
			"raw_url", "",
			"contents_url", "",
		))
	}
	page, headers := restPaginate(c, objs, fmt.Sprintf("/api/v3/repos/%s/%s/pulls/%d/files", repo.Owner, repo.Name, number))
	out := make([]interface{}, 0, len(page))
	for _, o := range page {
		out = append(out, o)
	}
	c.SendJSONH(200, out, headers)
}

func handleMergePull(c *Ctx, repo *RepoRec, number int) {
	pull := restPullOf(c, repo, number)
	if pull == nil {
		return
	}
	if _, ok := restBodyObject(c); !ok {
		return
	}
	if pull.State == "closed" && !pull.Merged {
		c.SendJSON(405, NewOrderedObj("message", "Pull Request is not mergeable"))
		return
	}
	sha, merged, _ := MergePullRecord(repo, pull)
	if !merged {
		c.SendJSON(405, NewOrderedObj("message", "Pull Request is not mergeable"))
		return
	}
	c.SendJSON(200, NewOrderedObj(
		"sha", sha,
		"merged", true,
		"message", "Pull Request successfully merged",
	))
}

// ---------------------------------------------------------------------------
// Router

// HandleRest routes one /api/v3 request. It returns false only when the
// path is not under /api/v3 at all (app.ts then answers a plain 404).
func HandleRest(c *Ctx) bool {
	seg := c.Segments
	if len(seg) < 2 || seg[0] != "api" || seg[1] != "v3" {
		return false
	}
	rest := seg[2:]
	method := c.Method

	if len(rest) == 0 {
		if method == "GET" {
			handleRoot(c)
		} else {
			restNotFound(c)
		}
		return true
	}
	if rest[0] == "meta" && len(rest) == 1 {
		if method == "GET" {
			handleMeta(c)
		} else {
			restNotFound(c)
		}
		return true
	}
	if rest[0] == "user" && len(rest) == 1 {
		if method == "GET" {
			handleUser(c)
		} else {
			restNotFound(c)
		}
		return true
	}
	if rest[0] == "user" && len(rest) >= 2 && rest[1] == "repos" && len(rest) == 2 {
		if method == "POST" {
			handleCreateRepo(c)
		} else if method == "GET" {
			handleListUserRepos(c)
		} else {
			restNotFound(c)
		}
		return true
	}
	if rest[0] == "repos" && len(rest) >= 3 {
		owner := rest[1]
		name := rest[2]
		tail := rest[3:]
		if len(tail) == 0 {
			switch method {
			case "GET":
				handleGetRepo(c, owner, name)
			case "PATCH":
				handlePatchRepo(c, owner, name)
			case "DELETE":
				handleDeleteRepo(c, owner, name)
			default:
				restNotFound(c)
			}
			return true
		}
		repo := restPathRepo(c, owner, name)
		if repo == nil {
			return true
		}

		if tail[0] == "branches" {
			if len(tail) == 1 && method == "GET" {
				handleListBranches(c, repo)
			} else if len(tail) >= 2 && method == "GET" {
				handleGetBranch(c, repo, strings.Join(tail[1:], "/"))
			} else {
				restNotFound(c)
			}
			return true
		}
		if tail[0] == "git" && len(tail) >= 2 && tail[1] == "refs" {
			if len(tail) == 2 && method == "POST" {
				handleCreateRef(c, repo)
			} else if len(tail) >= 3 && method == "GET" {
				handleGetRef(c, repo, strings.Join(tail[2:], "/"))
			} else {
				restNotFound(c)
			}
			return true
		}
		if tail[0] == "commits" && len(tail) == 1 {
			if method == "GET" {
				handleListCommits(c, repo)
			} else {
				restNotFound(c)
			}
			return true
		}
		if tail[0] == "contents" {
			path := strings.Join(tail[1:], "/")
			switch method {
			case "GET":
				handleGetContents(c, repo, path)
			case "PUT":
				handlePutContents(c, repo, path)
			case "DELETE":
				handleDeleteContents(c, repo, path)
			default:
				restNotFound(c)
			}
			return true
		}
		if tail[0] == "labels" {
			if len(tail) == 1 && method == "POST" {
				handleCreateLabel(c, repo)
			} else if len(tail) == 1 && method == "GET" {
				handleListLabels(c, repo)
			} else {
				restNotFound(c)
			}
			return true
		}
		if tail[0] == "issues" {
			if len(tail) == 1 {
				switch method {
				case "POST":
					handleCreateIssue(c, repo)
				case "GET":
					handleListIssues(c, repo)
				default:
					restNotFound(c)
				}
				return true
			}
			number, numOk := restJSParseInt(tail[1])
			if !numOk {
				restNotFound(c)
				return true
			}
			if len(tail) == 2 {
				switch method {
				case "GET":
					handleGetIssue(c, repo, number)
				case "PATCH":
					handlePatchIssue(c, repo, number)
				default:
					restNotFound(c)
				}
				return true
			}
			if len(tail) == 3 && tail[2] == "comments" {
				switch method {
				case "POST":
					handleCreateComment(c, repo, number)
				case "GET":
					handleListComments(c, repo, number)
				default:
					restNotFound(c)
				}
				return true
			}
			restNotFound(c)
			return true
		}
		if tail[0] == "pulls" {
			if len(tail) == 1 {
				switch method {
				case "POST":
					handleCreatePull(c, repo)
				case "GET":
					handleListPulls(c, repo)
				default:
					restNotFound(c)
				}
				return true
			}
			number, numOk := restJSParseInt(tail[1])
			if !numOk {
				restNotFound(c)
				return true
			}
			if len(tail) == 2 {
				if method == "GET" {
					handleGetPull(c, repo, number)
				} else {
					restNotFound(c)
				}
				return true
			}
			if len(tail) == 3 && tail[2] == "files" && method == "GET" {
				handlePullFiles(c, repo, number)
				return true
			}
			if len(tail) == 3 && tail[2] == "merge" && method == "PUT" {
				handleMergePull(c, repo, number)
				return true
			}
			restNotFound(c)
			return true
		}
		restNotFound(c)
		return true
	}
	restNotFound(c)
	return true
}
