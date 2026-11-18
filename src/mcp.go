package main

// The MCP surface: a stateless JSON-RPC endpoint speaking the modern protocol era (server/discover
// first, legacy initialize tolerated), with six tools that act on the same store as REST and
// GraphQL. Tool results carry both a text content block and structuredContent.
// Ported faithfully from entry/mcp.ts.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const mcpServerName = "makefun-forge"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpToolProp struct {
	name        string
	typ         string
	description string
}

type mcpToolDef struct {
	name        string
	description string
	properties  []mcpToolProp
	required    []string
}

var mcpTools = []mcpToolDef{
	{
		name:        "create_repo",
		description: "Create a new repository for the authenticated user and return it.",
		properties: []mcpToolProp{
			{name: "name", typ: "string", description: "The repository name."},
			{name: "description", typ: "string", description: "A short description of the repository."},
			{name: "private", typ: "boolean", description: "Whether the repository is private."},
		},
		required: []string{"name"},
	},
	{
		name:        "get_repo",
		description: "Get a repository by owner and name.",
		properties: []mcpToolProp{
			{name: "owner", typ: "string", description: "The repository owner login."},
			{name: "repo", typ: "string", description: "The repository name."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository and return it.",
		properties: []mcpToolProp{
			{name: "owner", typ: "string", description: "The repository owner login."},
			{name: "repo", typ: "string", description: "The repository name."},
			{name: "title", typ: "string", description: "The issue title."},
			{name: "body", typ: "string", description: "The issue body."},
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name:        "list_issues",
		description: "List the issues of a repository, optionally filtered by state.",
		properties: []mcpToolProp{
			{name: "owner", typ: "string", description: "The repository owner login."},
			{name: "repo", typ: "string", description: "The repository name."},
			{name: "state", typ: "string", description: "Filter by state: open, closed or all."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_pull_request",
		description: "Create a pull request from a head branch into a base branch and return it.",
		properties: []mcpToolProp{
			{name: "owner", typ: "string", description: "The repository owner login."},
			{name: "repo", typ: "string", description: "The repository name."},
			{name: "title", typ: "string", description: "The pull request title."},
			{name: "head", typ: "string", description: "The head branch to merge from."},
			{name: "base", typ: "string", description: "The base branch to merge into."},
			{name: "body", typ: "string", description: "The pull request body."},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:        "merge_pull_request",
		description: "Squash-merge a pull request and report the merge.",
		properties: []mcpToolProp{
			{name: "owner", typ: "string", description: "The repository owner login."},
			{name: "repo", typ: "string", description: "The repository name."},
			{name: "pull_number", typ: "integer", description: "The pull request number."},
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

func mcpToolJSON(tool mcpToolDef) *ojson {
	props := newOJSON()
	for _, p := range tool.properties {
		po := newOJSON()
		po.set("type", p.typ)
		po.set("description", p.description)
		props.set(p.name, po)
	}
	schema := newOJSON()
	schema.set("type", "object")
	schema.set("properties", props)
	required := make([]interface{}, 0, len(tool.required))
	for _, r := range tool.required {
		required = append(required, r)
	}
	schema.set("required", required)
	out := newOJSON()
	out.set("name", tool.name)
	out.set("description", tool.description)
	out.set("inputSchema", schema)
	return out
}

// Built once so tools/list is byte-identical across calls.
var mcpToolsListJSON []byte

func init() {
	tools := make([]interface{}, 0, len(mcpTools))
	for _, tool := range mcpTools {
		tools = append(tools, mcpToolJSON(tool))
	}
	mcpToolsListJSON = marshalOJSON(tools)
}

// ---------------------------------------------------------------------------
// REST serializers reused by the tools (ported from entry/rest.ts, kept local
// so this file does not depend on rest.go's naming).

func mcpRepoJSON(repo *RepoRec, origin string) *ojson {
	owner := newOJSON()
	owner.set("login", repo.Owner)
	owner.set("id", ViewerID)
	owner.set("type", "User")
	openCount := 0
	for _, e := range repo.Entries {
		if e.State == "open" {
			openCount++
		}
	}
	gitHost := origin
	gitHost = strings.TrimPrefix(gitHost, "https://")
	gitHost = strings.TrimPrefix(gitHost, "http://")
	permissions := newOJSON()
	permissions.set("admin", true)
	permissions.set("maintain", true)
	permissions.set("push", true)
	permissions.set("triage", true)
	permissions.set("pull", true)
	out := newOJSON()
	out.set("id", repo.ID)
	out.set("node_id", GlobalID("repo", repo.ID))
	out.set("name", repo.Name)
	out.set("full_name", repo.Owner+"/"+repo.Name)
	out.set("private", repo.IsPrivate)
	out.set("owner", owner)
	out.set("description", repo.Description)
	out.set("fork", false)
	out.set("default_branch", repo.DefaultBranch)
	out.set("created_at", repo.CreatedAt)
	out.set("updated_at", repo.UpdatedAt)
	out.set("pushed_at", repo.UpdatedAt)
	out.set("html_url", origin+"/"+repo.Owner+"/"+repo.Name)
	out.set("clone_url", origin+"/"+repo.Owner+"/"+repo.Name+".git")
	out.set("git_url", "git://"+gitHost+"/"+repo.Owner+"/"+repo.Name+".git")
	out.set("size", 0)
	out.set("stargazers_count", 0)
	out.set("watchers_count", 0)
	out.set("forks_count", 0)
	out.set("open_issues_count", openCount)
	if repo.IsPrivate {
		out.set("visibility", "private")
	} else {
		out.set("visibility", "public")
	}
	out.set("permissions", permissions)
	return out
}

func mcpLabelJSON(repo *RepoRec, name string) *ojson {
	color := "ededed"
	description := ""
	if label, ok := repo.Labels[name]; ok && label != nil {
		color = label.Color
		description = label.Description
	}
	out := newOJSON()
	out.set("id", 0)
	out.set("node_id", "")
	out.set("name", name)
	out.set("color", color)
	out.set("description", description)
	out.set("default", false)
	return out
}

func mcpViewerJSON() *ojson {
	out := newOJSON()
	out.set("login", ViewerLogin)
	out.set("id", ViewerID)
	out.set("type", "User")
	return out
}

func mcpIssueJSON(repo *RepoRec, issue *EntryRec, origin string) *ojson {
	labels := make([]interface{}, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		labels = append(labels, mcpLabelJSON(repo, name))
	}
	out := newOJSON()
	out.set("id", issue.ID)
	out.set("node_id", GlobalID("issue", issue.ID))
	out.set("number", issue.Number)
	out.set("title", issue.Title)
	out.set("body", issue.Body)
	out.set("state", issue.State)
	out.set("labels", labels)
	out.set("user", mcpViewerJSON())
	out.set("author", mcpViewerJSON())
	out.set("comments", len(issue.Comments))
	out.set("created_at", issue.CreatedAt)
	out.set("updated_at", issue.UpdatedAt)
	if issue.State == "closed" {
		out.set("closed_at", issue.UpdatedAt)
	} else {
		out.set("closed_at", nil)
	}
	out.set("html_url", fmt.Sprintf("%s/%s/%s/issues/%d", origin, repo.Owner, repo.Name, issue.Number))
	out.set("repository_url", fmt.Sprintf("%s/api/v3/repos/%s/%s", origin, repo.Owner, repo.Name))
	return out
}

func mcpPullJSON(repo *RepoRec, pull *EntryRec, origin string) *ojson {
	headSha := PullHeadSha(repo, pull)
	baseSha := PullBaseSha(repo, pull)
	head := newOJSON()
	head.set("ref", pull.HeadRef)
	head.set("sha", headSha)
	head.set("label", repo.Owner+":"+pull.HeadRef)
	head.set("user", mcpViewerJSON())
	head.set("repo", mcpRepoJSON(repo, origin))
	base := newOJSON()
	base.set("ref", pull.BaseRef)
	base.set("sha", baseSha)
	base.set("label", repo.Owner+":"+pull.BaseRef)
	base.set("user", mcpViewerJSON())
	base.set("repo", mcpRepoJSON(repo, origin))
	out := newOJSON()
	out.set("id", pull.ID)
	out.set("node_id", GlobalID("pull", pull.ID))
	out.set("number", pull.Number)
	out.set("title", pull.Title)
	out.set("body", pull.Body)
	out.set("state", pull.State)
	out.set("merged", pull.Merged)
	if pull.HasMergedAt {
		out.set("merged_at", pull.MergedAt)
	} else {
		out.set("merged_at", nil)
	}
	if pull.HasMergeSha {
		out.set("merge_commit_sha", pull.MergeCommitSha)
	} else {
		out.set("merge_commit_sha", nil)
	}
	out.set("draft", false)
	out.set("user", mcpViewerJSON())
	out.set("author", mcpViewerJSON())
	out.set("head", head)
	out.set("base", base)
	out.set("comments", len(pull.Comments))
	out.set("created_at", pull.CreatedAt)
	out.set("updated_at", pull.UpdatedAt)
	if pull.State == "closed" {
		out.set("closed_at", pull.UpdatedAt)
	} else {
		out.set("closed_at", nil)
	}
	out.set("html_url", fmt.Sprintf("%s/%s/%s/pull/%d", origin, repo.Owner, repo.Name, pull.Number))
	return out
}

// ---------------------------------------------------------------------------
// JSON-RPC plumbing

type mcpRpcResult struct {
	status  int
	payload *ojson
}

func mcpRPCError(id interface{}, code int, message string) mcpRpcResult {
	errObj := newOJSON()
	errObj.set("code", code)
	errObj.set("message", message)
	out := newOJSON()
	out.set("jsonrpc", "2.0")
	out.set("id", id)
	out.set("error", errObj)
	return mcpRpcResult{status: 200, payload: out}
}

func mcpRPCOk(id interface{}, result interface{}) mcpRpcResult {
	out := newOJSON()
	out.set("jsonrpc", "2.0")
	out.set("id", id)
	out.set("result", result)
	return mcpRpcResult{status: 200, payload: out}
}

func mcpToolOk(value interface{}) *ojson {
	contentItem := newOJSON()
	contentItem.set("type", "text")
	contentItem.set("text", string(marshalOJSON(value)))
	out := newOJSON()
	out.set("content", []interface{}{contentItem})
	out.set("structuredContent", value)
	out.set("isError", false)
	return out
}

func mcpToolError(message string) *ojson {
	contentItem := newOJSON()
	contentItem.set("type", "text")
	contentItem.set("text", message)
	out := newOJSON()
	out.set("content", []interface{}{contentItem})
	out.set("isError", true)
	return out
}

func mcpFindRepo(args map[string]interface{}) *RepoRec {
	owner, _ := args["owner"].(string)
	name, _ := args["repo"].(string)
	if owner != ViewerLogin || S == nil {
		return nil
	}
	return S.GetRepo(owner, name)
}

func mcpFindTool(name string) *mcpToolDef {
	for i := range mcpTools {
		if mcpTools[i].name == name {
			return &mcpTools[i]
		}
	}
	return nil
}

// mcpJSNumber mirrors JS Number(value) for the argument shapes tools receive.
func mcpJSNumber(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	}
	return 0, false
}

func mcpCallTool(c *Ctx, name string, args map[string]interface{}) *ojson {
	def := mcpFindTool(name)
	if def == nil {
		return mcpToolError("Unknown tool: " + name)
	}
	for _, required := range def.required {
		if v, present := args[required]; !present || v == nil {
			return mcpToolError("Missing required argument: " + required)
		}
	}
	switch name {
	case "create_repo":
		repoName := gqlJSString(args["name"])
		if S.GetRepo(ViewerLogin, repoName) != nil {
			return mcpToolError(fmt.Sprintf("Repository '%s' already exists", repoName))
		}
		description, _ := args["description"].(string)
		private, _ := args["private"].(bool)
		repo := S.CreateRepo(ViewerLogin, repoName, description, private)
		return mcpToolOk(mcpRepoJSON(repo, c.Origin))
	case "get_repo":
		repo := mcpFindRepo(args)
		if repo == nil {
			return mcpToolError("Repository not found")
		}
		return mcpToolOk(mcpRepoJSON(repo, c.Origin))
	case "create_issue":
		repo := mcpFindRepo(args)
		if repo == nil {
			return mcpToolError("Repository not found")
		}
		body, _ := args["body"].(string)
		issue := CreateIssueRecord(repo, gqlJSString(args["title"]), body, []string{})
		return mcpToolOk(mcpIssueJSON(repo, issue, c.Origin))
	case "list_issues":
		repo := mcpFindRepo(args)
		if repo == nil {
			return mcpToolError("Repository not found")
		}
		state := "open"
		if s, ok := args["state"].(string); ok {
			state = s
		}
		issues := []*EntryRec{}
		for _, entry := range repo.Entries {
			if entry.Kind != "issue" {
				continue
			}
			if state == "open" && entry.State != "open" {
				continue
			}
			if state == "closed" && entry.State != "closed" {
				continue
			}
			issues = append(issues, entry)
		}
		sort.SliceStable(issues, func(i, j int) bool { return issues[i].CreatedSeq < issues[j].CreatedSeq })
		out := make([]interface{}, 0, len(issues))
		for _, issue := range issues {
			out = append(out, mcpIssueJSON(repo, issue, c.Origin))
		}
		return mcpToolOk(out)
	case "create_pull_request":
		repo := mcpFindRepo(args)
		if repo == nil {
			return mcpToolError("Repository not found")
		}
		body, _ := args["body"].(string)
		pull, errStr := CreatePullRecord(repo, gqlJSString(args["title"]), body, gqlJSString(args["head"]), gqlJSString(args["base"]))
		if errStr != "" {
			return mcpToolError("Could not create pull request: " + errStr)
		}
		return mcpToolOk(mcpPullJSON(repo, pull, c.Origin))
	case "merge_pull_request":
		repo := mcpFindRepo(args)
		if repo == nil {
			return mcpToolError("Repository not found")
		}
		number, finite := mcpJSNumber(args["pull_number"])
		var entry *EntryRec
		if finite {
			entry = repo.Entries[int(number)]
		}
		if entry == nil || entry.Kind != "pull" {
			return mcpToolError("Pull request not found")
		}
		sha, ok, _ := MergePullRecord(repo, entry)
		if !ok {
			return mcpToolError("Pull request is not mergeable")
		}
		result := newOJSON()
		result.set("merged", true)
		result.set("sha", sha)
		result.set("message", "Pull Request successfully merged")
		result.set("number", entry.Number)
		result.set("state", "closed")
		return mcpToolOk(result)
	}
	return mcpToolError("Unknown tool: " + name)
}

func mcpDispatch(c *Ctx, request map[string]interface{}) mcpRpcResult {
	method, _ := request["method"].(string)
	id := request["id"] // nil when absent, matching `id ?? null`
	params := asRecord(request["params"])
	if params == nil {
		params = map[string]interface{}{}
	}

	if method == "server/discover" {
		versions := make([]interface{}, 0, len(mcpSupportedVersions))
		for _, v := range mcpSupportedVersions {
			versions = append(versions, v)
		}
		capabilities := newOJSON()
		capabilities.set("tools", newOJSON())
		serverInfo := newOJSON()
		serverInfo.set("name", mcpServerName)
		serverInfo.set("version", mcpServerVersion)
		metaInner := newOJSON()
		metaInner.set("name", mcpServerName)
		metaInner.set("version", mcpServerVersion)
		meta := newOJSON()
		meta.set("io.modelcontextprotocol/serverInfo", metaInner)
		result := newOJSON()
		result.set("supportedVersions", versions)
		result.set("capabilities", capabilities)
		result.set("serverInfo", serverInfo)
		result.set("_meta", meta)
		return mcpRPCOk(id, result)
	}
	if method == "initialize" {
		requested, _ := params["protocolVersion"].(string)
		protocolVersion := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if v == requested {
				protocolVersion = requested
				break
			}
		}
		capabilities := newOJSON()
		capabilities.set("tools", newOJSON())
		serverInfo := newOJSON()
		serverInfo.set("name", mcpServerName)
		serverInfo.set("version", mcpServerVersion)
		result := newOJSON()
		result.set("protocolVersion", protocolVersion)
		result.set("capabilities", capabilities)
		result.set("serverInfo", serverInfo)
		return mcpRPCOk(id, result)
	}
	if method == "ping" {
		return mcpRPCOk(id, newOJSON())
	}
	if method == "tools/list" {
		result := newOJSON()
		result.set("tools", json.RawMessage(mcpToolsListJSON))
		return mcpRpcResult{status: 200, payload: func() *ojson {
			out := newOJSON()
			out.set("jsonrpc", "2.0")
			out.set("id", id)
			out.set("result", result)
			return out
		}()}
	}
	if method == "tools/call" {
		name, _ := params["name"].(string)
		if mcpFindTool(name) == nil {
			return mcpRPCError(id, -32602, "Unknown tool: "+name)
		}
		args := asRecord(params["arguments"])
		if args == nil {
			args = map[string]interface{}{}
		}
		return mcpRPCOk(id, mcpCallTool(c, name, args))
	}
	return mcpRPCError(id, -32601, "Method not found: "+method)
}

func mcpSendRPCError(c *Ctx, status int, code int, message string) {
	errObj := newOJSON()
	errObj.set("code", code)
	errObj.set("message", message)
	out := newOJSON()
	out.set("jsonrpc", "2.0")
	out.set("id", nil)
	out.set("error", errObj)
	sendOrderedJSON(c, status, out)
}

// HandleMcp serves POST /mcp. Parse first: an unparseable body is a -32700
// before auth is even considered.
func HandleMcp(c *Ctx, authorized bool) {
	var parsed interface{}
	if err := json.Unmarshal(c.Body, &parsed); err != nil {
		mcpSendRPCError(c, 400, -32700, "Parse error")
		return
	}
	if !authorized {
		mcpSendRPCError(c, 401, -32001, "Unauthorized")
		return
	}
	if batch, ok := parsed.([]interface{}); ok {
		responses := make([]interface{}, 0, len(batch))
		for _, entry := range batch {
			record := asRecord(entry)
			if record == nil {
				continue
			}
			if _, hasID := record["id"]; !hasID {
				continue // notification inside a batch: no response
			}
			result := mcpDispatch(c, record)
			if result.payload != nil {
				responses = append(responses, result.payload)
			}
		}
		sendOrderedJSON(c, 200, responses)
		return
	}
	record := asRecord(parsed)
	if record == nil {
		mcpSendRPCError(c, 400, -32600, "Invalid request")
		return
	}
	if _, hasID := record["id"]; !hasID {
		// A notification (e.g. notifications/initialized): accepted, no body.
		c.Send(202, "application/json; charset=utf-8", []byte(""))
		return
	}
	result := mcpDispatch(c, record)
	if result.payload == nil {
		c.Send(202, "application/json; charset=utf-8", []byte(""))
		return
	}
	sendOrderedJSON(c, result.status, result.payload)
}
