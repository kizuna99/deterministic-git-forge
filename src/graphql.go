package main

// The GraphQL surface: a small parser and executor for the query shapes the suite (and gh-style
// clients) send — viewer, repository, node, issues and pull requests with cursor pagination,
// the five mutations, batching, and the slice of introspection that gets probed.
// Ported faithfully from entry/graphql.ts.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Ordered JSON (mirrors JS object insertion order + JSON.stringify escaping)

type ojson struct {
	keys []string
	vals map[string]interface{}
}

func newOJSON() *ojson {
	return &ojson{vals: map[string]interface{}{}}
}

func (o *ojson) set(key string, value interface{}) *ojson {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
	return o
}

func quoteJSONString(s string) string {
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

func marshalOJSON(v interface{}) []byte {
	var b strings.Builder
	writeOJSONValue(&b, v)
	return []byte(b.String())
}

func writeOJSONValue(b *strings.Builder, v interface{}) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case *ojson:
		b.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(quoteJSONString(k))
			b.WriteByte(':')
			writeOJSONValue(b, t.vals[k])
		}
		b.WriteByte('}')
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(quoteJSONString(k))
			b.WriteByte(':')
			writeOJSONValue(b, t[k])
		}
		b.WriteByte('}')
	case []interface{}:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeOJSONValue(b, item)
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(quoteJSONString(item))
		}
		b.WriteByte(']')
	case string:
		b.WriteString(quoteJSONString(t))
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(jsNumberString(t))
	case json.Number:
		b.WriteString(t.String())
	case json.RawMessage:
		b.Write(t)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			b.WriteString("null")
		} else {
			b.Write(raw)
		}
	}
}

// jsNumberString renders a float the way JS Number -> String does for the
// values this surface produces (integral floats print without a fraction).
func jsNumberString(f float64) string {
	if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// gqlJSString mirrors JS String(value) for JSON-derived values.
func gqlJSString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.Number:
		return t.String()
	default:
		return string(marshalOJSON(v))
	}
}

// gqlNum reports whether v is a JSON number, and its value.
func gqlNum(v interface{}) (float64, bool) {
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
	}
	return 0, false
}

func isRecordValue(v interface{}) bool {
	_, ok := v.(map[string]interface{})
	return ok
}

func asRecord(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

// sendOrderedJSON mirrors Ctx.sendJson in the TS app: JSON body + "\n".
func sendOrderedJSON(c *Ctx, status int, v interface{}) {
	body := marshalOJSON(v)
	body = append(body, '\n')
	c.Send(status, "application/json; charset=utf-8", body)
}

// ---------------------------------------------------------------------------
// Lexer

type gqlTokenKind int

const (
	gqlTokName gqlTokenKind = iota
	gqlTokString
	gqlTokInt
	gqlTokFloat
	gqlTokPunct
	gqlTokSpread
	gqlTokEOF
)

type gqlToken struct {
	kind  gqlTokenKind
	str   string
	num   float64
	punct byte
}

func gqlIsNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func gqlIsNameChar(c byte) bool {
	return gqlIsNameStart(c) || (c >= '0' && c <= '9')
}

func gqlTokenize(source string) []gqlToken {
	tokens := []gqlToken{}
	i := 0
	for i < len(source) {
		c := source[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || strings.HasPrefix(source[i:], "\uFEFF") {
			if strings.HasPrefix(source[i:], "\uFEFF") {
				i += len("\uFEFF")
			} else {
				i++
			}
			continue
		}
		if c == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(source[i:], "...") {
			tokens = append(tokens, gqlToken{kind: gqlTokSpread})
			i += 3
			continue
		}
		if strings.IndexByte("!$():=@[]{}|", c) >= 0 {
			tokens = append(tokens, gqlToken{kind: gqlTokPunct, punct: c})
			i++
			continue
		}
		if c == '"' {
			if strings.HasPrefix(source[i:], `"""`) {
				end := strings.Index(source[i+3:], `"""`)
				var value string
				if end < 0 {
					value = source[i+3:]
					i = len(source)
				} else {
					value = source[i+3 : i+3+end]
					i = i + 3 + end + 3
				}
				tokens = append(tokens, gqlToken{kind: gqlTokString, str: value})
				continue
			}
			j := i + 1
			var sb strings.Builder
			for j < len(source) && source[j] != '"' {
				if source[j] == '\\' && j+1 < len(source) {
					esc := source[j+1]
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					default:
						sb.WriteByte(esc)
					}
					j += 2
				} else {
					sb.WriteByte(source[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: gqlTokString, str: sb.String()})
			i = j + 1
			continue
		}
		if (c >= '0' && c <= '9') || c == '-' {
			j := i
			for j < len(source) && strings.IndexByte("0123456789.eE+-", source[j]) >= 0 {
				j++
			}
			text := source[i:j]
			num, _ := strconv.ParseFloat(text, 64)
			if strings.ContainsAny(text, ".eE") {
				tokens = append(tokens, gqlToken{kind: gqlTokFloat, num: num})
			} else {
				tokens = append(tokens, gqlToken{kind: gqlTokInt, num: num})
			}
			i = j
			continue
		}
		if i < len(source) && gqlIsNameStart(source[i]) {
			j := i
			for j < len(source) && gqlIsNameChar(source[j]) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: gqlTokName, str: source[i:j]})
			i = j
			continue
		}
		i++ // unknown character: skip rather than fail the whole document
	}
	tokens = append(tokens, gqlToken{kind: gqlTokEOF})
	return tokens
}

// ---------------------------------------------------------------------------
// Parser

type gqlValueKind int

const (
	gqlValVariable gqlValueKind = iota
	gqlValString
	gqlValInt
	gqlValFloat
	gqlValBool
	gqlValNull
	gqlValEnum
	gqlValList
	gqlValObject
)

type gqlObjField struct {
	name  string
	value *gqlValue
}

type gqlValue struct {
	kind  gqlValueKind
	str   string
	num   float64
	b     bool
	items []*gqlValue
	obj   []gqlObjField
}

type gqlFieldNode struct {
	name      string
	alias     string
	hasAlias  bool
	args      []gqlObjField
	selection []gqlSelNode
	hasSel    bool
}

type gqlSelKind int

const (
	gqlSelField gqlSelKind = iota
	gqlSelInline
	gqlSelSpread
)

type gqlSelNode struct {
	kind          gqlSelKind
	field         *gqlFieldNode
	typeCondition string
	hasTypeCond   bool
	selection     []gqlSelNode
	spreadName    string
}

type gqlOperation struct {
	opType    string // "query" or "mutation"
	name      string
	hasName   bool
	selection []gqlSelNode
}

type gqlFragment struct {
	typeCondition string
	hasTypeCond   bool
	selection     []gqlSelNode
}

type gqlDocument struct {
	operations []gqlOperation
	fragments  map[string]*gqlFragment
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken {
	return p.tokens[p.pos]
}

func (p *gqlParser) next() gqlToken {
	t := p.tokens[p.pos]
	p.pos++
	return t
}

func (p *gqlParser) expectPunct(v byte) {
	t := p.next()
	if t.kind != gqlTokPunct || t.punct != v {
		panic(fmt.Sprintf("expected '%c'", v))
	}
}

func (p *gqlParser) skipPunct(v byte) bool {
	t := p.peek()
	if t.kind == gqlTokPunct && t.punct == v {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) peekPunct(v byte) bool {
	t := p.peek()
	return t.kind == gqlTokPunct && t.punct == v
}

func (p *gqlParser) expectName() string {
	t := p.next()
	if t.kind != gqlTokName {
		panic("expected a name")
	}
	return t.str
}

func (p *gqlParser) parseDocument() *gqlDocument {
	doc := &gqlDocument{fragments: map[string]*gqlFragment{}}
	for p.peek().kind != gqlTokEOF {
		token := p.peek()
		if token.kind == gqlTokPunct && token.punct == '{' {
			doc.operations = append(doc.operations, gqlOperation{opType: "query", selection: p.parseSelectionSet()})
			continue
		}
		if token.kind == gqlTokName && (token.str == "query" || token.str == "mutation" || token.str == "subscription") {
			p.next()
			op := gqlOperation{opType: "query"}
			if token.str == "mutation" {
				op.opType = "mutation"
			}
			if p.peek().kind == gqlTokName {
				op.name = p.expectName()
				op.hasName = true
			}
			if p.peekPunct('(') {
				p.skipBalancedParens()
			}
			p.skipDirectives()
			op.selection = p.parseSelectionSet()
			doc.operations = append(doc.operations, op)
			continue
		}
		if token.kind == gqlTokName && token.str == "fragment" {
			p.next()
			fragName := p.expectName()
			frag := &gqlFragment{}
			if p.peek().kind == gqlTokName && p.peek().str == "on" {
				p.next()
				frag.typeCondition = p.expectName()
				frag.hasTypeCond = true
			}
			p.skipDirectives()
			frag.selection = p.parseSelectionSet()
			doc.fragments[fragName] = frag
			continue
		}
		p.next() // tolerate anything else at the top level
	}
	return doc
}

func (p *gqlParser) skipBalancedParens() {
	depth := 0
	for {
		token := p.next()
		if token.kind == gqlTokEOF {
			return
		}
		if token.kind == gqlTokPunct && token.punct == '(' {
			depth++
		}
		if token.kind == gqlTokPunct && token.punct == ')' {
			depth--
			if depth == 0 {
				return
			}
		}
	}
}

func (p *gqlParser) skipDirectives() {
	for p.peekPunct('@') {
		p.next()
		p.expectName()
		if p.peekPunct('(') {
			p.skipBalancedParens()
		}
	}
}

func (p *gqlParser) parseSelectionSet() []gqlSelNode {
	p.expectPunct('{')
	selections := []gqlSelNode{}
	for !p.peekPunct('}') {
		if p.peek().kind == gqlTokEOF {
			panic("unterminated selection set")
		}
		if p.peek().kind == gqlTokSpread {
			p.next()
			if p.peek().kind == gqlTokName && p.peek().str == "on" {
				p.next()
				typeCondition := p.expectName()
				p.skipDirectives()
				selections = append(selections, gqlSelNode{
					kind: gqlSelInline, typeCondition: typeCondition, hasTypeCond: true,
					selection: p.parseSelectionSet(),
				})
			} else {
				name := p.expectName()
				p.skipDirectives()
				selections = append(selections, gqlSelNode{kind: gqlSelSpread, spreadName: name})
			}
			continue
		}
		selections = append(selections, gqlSelNode{kind: gqlSelField, field: p.parseField()})
	}
	p.expectPunct('}')
	return selections
}

func (p *gqlParser) parseField() *gqlFieldNode {
	name := p.expectName()
	f := &gqlFieldNode{name: name}
	if p.skipPunct(':') {
		f.alias = name
		f.hasAlias = true
		f.name = p.expectName()
	}
	if p.peekPunct('(') {
		p.next()
		for !p.peekPunct(')') {
			argName := p.expectName()
			p.expectPunct(':')
			f.args = append(f.args, gqlObjField{name: argName, value: p.parseValue()})
		}
		p.expectPunct(')')
	}
	p.skipDirectives()
	if p.peekPunct('{') {
		f.selection = p.parseSelectionSet()
		f.hasSel = true
	}
	return f
}

func (p *gqlParser) parseValue() *gqlValue {
	token := p.next()
	if token.kind == gqlTokPunct && token.punct == '$' {
		return &gqlValue{kind: gqlValVariable, str: p.expectName()}
	}
	if token.kind == gqlTokString {
		return &gqlValue{kind: gqlValString, str: token.str}
	}
	if token.kind == gqlTokInt {
		return &gqlValue{kind: gqlValInt, num: token.num}
	}
	if token.kind == gqlTokFloat {
		return &gqlValue{kind: gqlValFloat, num: token.num}
	}
	if token.kind == gqlTokPunct && token.punct == '[' {
		v := &gqlValue{kind: gqlValList}
		for !p.peekPunct(']') {
			v.items = append(v.items, p.parseValue())
		}
		p.expectPunct(']')
		return v
	}
	if token.kind == gqlTokPunct && token.punct == '{' {
		v := &gqlValue{kind: gqlValObject}
		for !p.peekPunct('}') {
			fieldName := p.expectName()
			p.expectPunct(':')
			v.obj = append(v.obj, gqlObjField{name: fieldName, value: p.parseValue()})
		}
		p.expectPunct('}')
		return v
	}
	if token.kind == gqlTokName {
		switch token.str {
		case "true":
			return &gqlValue{kind: gqlValBool, b: true}
		case "false":
			return &gqlValue{kind: gqlValBool, b: false}
		case "null":
			return &gqlValue{kind: gqlValNull}
		}
		return &gqlValue{kind: gqlValEnum, str: token.str}
	}
	panic("unexpected value token")
}

func gqlValueOf(value *gqlValue, variables map[string]interface{}) interface{} {
	switch value.kind {
	case gqlValVariable:
		return variables[value.str]
	case gqlValString, gqlValEnum:
		return value.str
	case gqlValInt, gqlValFloat:
		return value.num
	case gqlValBool:
		return value.b
	case gqlValNull:
		return nil
	case gqlValList:
		items := make([]interface{}, 0, len(value.items))
		for _, item := range value.items {
			items = append(items, gqlValueOf(item, variables))
		}
		return items
	case gqlValObject:
		out := map[string]interface{}{}
		for _, field := range value.obj {
			out[field.name] = gqlValueOf(field.value, variables)
		}
		return out
	}
	return nil
}

// ---------------------------------------------------------------------------
// Schema description for introspection

var gqlTypeFields = map[string][]string{
	"Query":    {"viewer", "repository", "node", "search", "__type", "__schema"},
	"Mutation": {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":     {"id", "login", "name", "email", "url"},
	"Viewer":   {"id", "login", "name", "email", "url"},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission",
		"hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests", "description", "isPrivate",
		"createdAt", "updatedAt", "url",
	},
	"Issue": {
		"id", "number", "title", "state", "body", "author", "labels", "comments", "createdAt",
		"updatedAt", "repository", "url", "closed",
	},
	"PullRequest": {
		"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName",
		"files", "author", "createdAt", "updatedAt", "repository", "url", "mergedAt", "mergeCommit",
	},
	"Ref":                              {"id", "name", "prefix", "target"},
	"PageInfo":                         {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"IssueConnection":                  {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection":            {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueComment":                     {"id", "body", "author", "createdAt", "url"},
	"Label":                            {"id", "name", "color", "description"},
	"LabelConnection":                  {"nodes", "edges", "pageInfo", "totalCount"},
	"CreateIssuePayload":               {"issue", "clientMutationId"},
	"CloseIssuePayload":                {"issue", "clientMutationId"},
	"CreatePullRequestPayload":         {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":          {"pullRequest", "clientMutationId"},
	"AddCommentPayload":                {"commentEdge", "subject", "clientMutationId"},
	"CommentEdge":                      {"node", "cursor"},
	"__Type":                           {"name", "kind", "fields", "enumValues", "description"},
	"__Field":                          {"name", "description", "args", "type"},
	"__EnumValue":                      {"name", "description"},
}

var gqlEnumTypes = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"Permission":             {"ADMIN", "WRITE", "READ", "TRIAGE", "MAINTAIN"},
	"OrderDirection":         {"ASC", "DESC"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"PullRequestMergeMethod": {"MERGE", "REBASE", "SQUASH"},
}

// ---------------------------------------------------------------------------
// Execution

type gqlExecCtx struct {
	variables map[string]interface{}
	fragments map[string]*gqlFragment
	errors    []string
	origin    string
}

// A resolved value tagged with its GraphQL type name for fragment matching.
type gqlTyped struct {
	typename string
	value    interface{}
}

func gqlTypedOf(typename string, value interface{}) gqlTyped {
	return gqlTyped{typename: typename, value: value}
}

type gqlEntryWrap struct {
	repo  *RepoRec
	entry *EntryRec
}

type gqlRefVal struct {
	name string
}

type gqlPageInfoVal struct {
	hasNextPage bool
	endCursor   interface{} // string or nil
}

type gqlConnVal struct {
	nodes    []gqlTyped
	pageInfo gqlTyped
}

type gqlFileVal struct {
	path      string
	additions int
	deletions int
}

type gqlLabelVal struct {
	name string
}

func gqlCursorOf(number int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", number)))
}

var gqlCursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

func gqlNumberFromCursor(cursor string) (int, bool) {
	decoded, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		// Buffer.from(..., 'base64') in Node tolerates missing padding.
		decoded2, err2 := base64.RawStdEncoding.DecodeString(cursor)
		if err2 != nil {
			return 0, false
		}
		decoded = decoded2
	}
	m := gqlCursorRe.FindStringSubmatch(string(decoded))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

func gqlIssueState(e *EntryRec) string {
	if e.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func gqlPullState(e *EntryRec) string {
	if e.Merged {
		return "MERGED"
	}
	if e.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

// Raw (unselected) rendering of a typed value, mirroring JSON.stringify of the
// TS value object — used for connection edges, whose node ignores selection.
func gqlRawValue(t gqlTyped) interface{} {
	switch v := t.value.(type) {
	case nil:
		return nil
	case string:
		return v
	case *RepoRec:
		return gqlRawRepo(v)
	case *gqlEntryWrap:
		if v.entry.Kind == "issue" {
			out := newOJSON()
			out.set("repo", gqlRawRepo(v.repo))
			out.set("issue", gqlRawEntry(v.entry))
			return out
		}
		out := newOJSON()
		out.set("repo", gqlRawRepo(v.repo))
		out.set("pull", gqlRawEntry(v.entry))
		return out
	case *gqlRefVal:
		out := newOJSON()
		out.set("name", v.name)
		return out
	case *gqlPageInfoVal:
		out := newOJSON()
		out.set("hasNextPage", v.hasNextPage)
		out.set("endCursor", v.endCursor)
		return out
	case *gqlConnVal:
		nodes := make([]interface{}, 0, len(v.nodes))
		for _, n := range v.nodes {
			nodes = append(nodes, gqlRawValue(n))
		}
		out := newOJSON()
		out.set("nodes", nodes)
		out.set("pageInfo", gqlRawValue(v.pageInfo))
		return out
	case *gqlFileVal:
		out := newOJSON()
		out.set("path", v.path)
		out.set("additions", v.additions)
		out.set("deletions", v.deletions)
		return out
	case *CommentRec:
		return gqlRawComment(v)
	case *gqlLabelVal:
		out := newOJSON()
		out.set("name", v.name)
		return out
	}
	return nil
}

func gqlRawRepo(r *RepoRec) *ojson {
	out := newOJSON()
	out.set("id", r.ID)
	out.set("owner", r.Owner)
	out.set("name", r.Name)
	out.set("description", r.Description)
	out.set("isPrivate", r.IsPrivate)
	out.set("defaultBranch", r.DefaultBranch)
	out.set("createdSeq", r.CreatedSeq)
	out.set("createdAt", r.CreatedAt)
	out.set("updatedAt", r.UpdatedAt)
	out.set("objects", newOJSON())
	out.set("refs", newOJSON())
	out.set("labels", newOJSON())
	out.set("labelsVersion", r.LabelsVersion)
	out.set("refsVersion", r.RefsVersion)
	out.set("entries", newOJSON())
	out.set("nextNumber", r.NextNumber)
	return out
}

func gqlRawComment(c *CommentRec) *ojson {
	out := newOJSON()
	out.set("id", c.ID)
	out.set("body", c.Body)
	out.set("authorLogin", c.AuthorLogin)
	out.set("createdSeq", c.CreatedSeq)
	out.set("createdAt", c.CreatedAt)
	return out
}

func gqlRawEntry(e *EntryRec) *ojson {
	comments := make([]interface{}, 0, len(e.Comments))
	for i := range e.Comments {
		comments = append(comments, gqlRawComment(&e.Comments[i]))
	}
	out := newOJSON()
	if e.Kind == "issue" {
		labels := make([]interface{}, 0, len(e.Labels))
		for _, l := range e.Labels {
			labels = append(labels, l)
		}
		out.set("kind", "issue")
		out.set("id", e.ID)
		out.set("number", e.Number)
		out.set("title", e.Title)
		out.set("body", e.Body)
		out.set("state", e.State)
		out.set("labels", labels)
		out.set("comments", comments)
		out.set("createdSeq", e.CreatedSeq)
		out.set("createdAt", e.CreatedAt)
		out.set("updatedAt", e.UpdatedAt)
		return out
	}
	out.set("kind", "pull")
	out.set("id", e.ID)
	out.set("number", e.Number)
	out.set("title", e.Title)
	out.set("body", e.Body)
	out.set("state", e.State)
	out.set("headRef", e.HeadRef)
	out.set("baseRef", e.BaseRef)
	out.set("headShaSnapshot", e.HeadShaSnapshot)
	out.set("baseShaSnapshot", e.BaseShaSnapshot)
	out.set("merged", e.Merged)
	if e.HasMergedAt {
		out.set("mergedAt", e.MergedAt)
	} else {
		out.set("mergedAt", nil)
	}
	if e.HasMergeSha {
		out.set("mergeCommitSha", e.MergeCommitSha)
	} else {
		out.set("mergeCommitSha", nil)
	}
	out.set("comments", comments)
	out.set("createdSeq", e.CreatedSeq)
	out.set("createdAt", e.CreatedAt)
	out.set("updatedAt", e.UpdatedAt)
	return out
}

func gqlResolveObject(ctx *gqlExecCtx, obj gqlTyped, selection []gqlSelNode, hasSel bool) interface{} {
	if !hasSel {
		return gqlRawValue(obj)
	}
	out := newOJSON()
	gqlExecSelectionInto(ctx, obj, selection, out)
	return out
}

func gqlExecSelectionInto(ctx *gqlExecCtx, obj gqlTyped, selection []gqlSelNode, out *ojson) {
	for _, node := range selection {
		switch node.kind {
		case gqlSelInline:
			if !node.hasTypeCond || node.typeCondition == obj.typename {
				gqlExecSelectionInto(ctx, obj, node.selection, out)
			}
		case gqlSelSpread:
			fragment, ok := ctx.fragments[node.spreadName]
			if ok && (!fragment.hasTypeCond || fragment.typeCondition == obj.typename) {
				gqlExecSelectionInto(ctx, obj, fragment.selection, out)
			}
		case gqlSelField:
			field := node.field
			key := field.name
			if field.hasAlias {
				key = field.alias
			}
			args := map[string]interface{}{}
			for _, arg := range field.args {
				args[arg.name] = gqlValueOf(arg.value, ctx.variables)
			}
			out.set(key, gqlResolveField(ctx, obj, field.name, args, field))
		}
	}
}

func gqlResolveField(ctx *gqlExecCtx, obj gqlTyped, field string, args map[string]interface{}, node *gqlFieldNode) interface{} {
	typ := obj.typename
	value := obj.value

	if field == "__typename" {
		return typ
	}

	if typ == "__Type" {
		typeName, _ := value.(string)
		switch field {
		case "name":
			return typeName
		case "kind":
			if _, ok := gqlEnumTypes[typeName]; ok {
				return "ENUM"
			}
			return "OBJECT"
		case "fields":
			fields, ok := gqlTypeFields[typeName]
			if !ok {
				return nil
			}
			out := make([]interface{}, 0, len(fields))
			for _, name := range fields {
				out = append(out, gqlResolveObject(ctx, gqlTypedOf("__Field", name), node.selection, node.hasSel))
			}
			return out
		case "enumValues":
			values, ok := gqlEnumTypes[typeName]
			if !ok {
				return nil
			}
			out := make([]interface{}, 0, len(values))
			for _, name := range values {
				out = append(out, gqlResolveObject(ctx, gqlTypedOf("__EnumValue", name), node.selection, node.hasSel))
			}
			return out
		case "description":
			return nil
		}
		ctx.errors = append(ctx.errors, fmt.Sprintf("Field '%s' doesn't exist on type '__Type'", field))
		return nil
	}
	if typ == "__Field" || typ == "__EnumValue" {
		switch field {
		case "name":
			s, _ := value.(string)
			return s
		case "description":
			return nil
		case "args":
			return []interface{}{}
		case "type":
			return nil
		}
		ctx.errors = append(ctx.errors, fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field, typ))
		return nil
	}

	switch typ {
	case "Query":
		return gqlResolveQuery(ctx, field, args, node)
	case "Mutation":
		return gqlResolveMutation(ctx, field, args, node)
	case "User", "Viewer":
		switch field {
		case "login":
			return ViewerLogin
		case "id":
			return GlobalID("user", ViewerID)
		case "name":
			return ViewerName
		case "email":
			return ViewerEmail
		case "url":
			return ctx.origin + "/" + ViewerLogin
		}
	case "Repository":
		return gqlResolveRepository(ctx, value.(*RepoRec), field, args, node)
	case "Issue":
		return gqlResolveIssue(ctx, value.(*gqlEntryWrap), field, node)
	case "PullRequest":
		return gqlResolvePull(ctx, value.(*gqlEntryWrap), field, args, node)
	case "Ref":
		rv := value.(*gqlRefVal)
		switch field {
		case "name":
			return rv.name
		case "id":
			return GlobalID("ref", 0)
		case "prefix":
			return "refs/heads/"
		}
	case "PageInfo":
		pi := value.(*gqlPageInfoVal)
		switch field {
		case "hasNextPage":
			return pi.hasNextPage
		case "hasPreviousPage":
			return false
		case "endCursor":
			return pi.endCursor
		case "startCursor":
			return pi.endCursor
		}
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection", "LabelConnection":
		conn := value.(*gqlConnVal)
		switch field {
		case "nodes":
			out := make([]interface{}, 0, len(conn.nodes))
			for _, n := range conn.nodes {
				out = append(out, gqlResolveObject(ctx, n, node.selection, node.hasSel))
			}
			return out
		case "pageInfo":
			return gqlResolveObject(ctx, conn.pageInfo, node.selection, node.hasSel)
		case "totalCount":
			return len(conn.nodes)
		case "edges":
			out := make([]interface{}, 0, len(conn.nodes))
			for _, n := range conn.nodes {
				edge := newOJSON()
				edge.set("node", gqlResolveObject(ctx, n, nil, false))
				edge.set("cursor", "")
				out = append(out, edge)
			}
			return out
		}
	case "PullRequestChangedFile":
		fv := value.(*gqlFileVal)
		switch field {
		case "path":
			return fv.path
		case "additions":
			return fv.additions
		case "deletions":
			return fv.deletions
		case "changeType":
			return "MODIFIED"
		}
	case "IssueComment":
		comment := value.(*CommentRec)
		switch field {
		case "id":
			return GlobalID("comment", comment.ID)
		case "body":
			return comment.Body
		case "author":
			return gqlResolveObject(ctx, gqlTypedOf("User", nil), node.selection, node.hasSel)
		}
	case "Label":
		lv := value.(*gqlLabelVal)
		switch field {
		case "name":
			return lv.name
		case "id":
			return GlobalID("label", 0)
		case "color":
			return "ededed"
		case "description":
			return ""
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		if field == "issue" {
			payload := value.(*gqlEntryWrap)
			return gqlResolveObject(ctx, gqlTypedOf("Issue", payload), node.selection, node.hasSel)
		}
		if field == "clientMutationId" {
			return nil
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field == "pullRequest" {
			payload := value.(*gqlEntryWrap)
			return gqlResolveObject(ctx, gqlTypedOf("PullRequest", payload), node.selection, node.hasSel)
		}
		if field == "clientMutationId" {
			return nil
		}
	case "AddCommentPayload":
		if field == "commentEdge" {
			comment := value.(*CommentRec)
			edgeObj := newOJSON()
			if node.hasSel {
				for _, sn := range node.selection {
					if sn.kind != gqlSelField {
						continue
					}
					if sn.field.name == "node" {
						key := "node"
						if sn.field.hasAlias {
							key = sn.field.alias
						}
						edgeObj.set(key, gqlResolveObject(ctx, gqlTypedOf("IssueComment", comment), sn.field.selection, sn.field.hasSel))
					} else if sn.field.name == "cursor" {
						key := "cursor"
						if sn.field.hasAlias {
							key = sn.field.alias
						}
						edgeObj.set(key, gqlCursorOf(comment.ID))
					}
				}
			}
			return edgeObj
		}
		if field == "clientMutationId" {
			return nil
		}
		if field == "subject" {
			return nil
		}
	}
	ctx.errors = append(ctx.errors, fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field, typ))
	return nil
}

// Store-wide lookups (all repos belong to the viewer in this forge).

func gqlAllRepos() []*RepoRec {
	if S == nil {
		return nil
	}
	return S.ListRepos(ViewerLogin)
}

func gqlRepoByID(id int) *RepoRec {
	for _, repo := range gqlAllRepos() {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func gqlFindEntryByID(numericID int) (*RepoRec, *EntryRec) {
	for _, repo := range gqlAllRepos() {
		for _, entry := range repo.Entries {
			if entry.ID == numericID {
				return repo, entry
			}
		}
	}
	return nil, nil
}

func gqlResolveQuery(ctx *gqlExecCtx, field string, args map[string]interface{}, node *gqlFieldNode) interface{} {
	switch field {
	case "viewer":
		return gqlResolveObject(ctx, gqlTypedOf("User", nil), node.selection, node.hasSel)
	case "__type":
		name, _ := args["name"].(string)
		_, inFields := gqlTypeFields[name]
		_, inEnums := gqlEnumTypes[name]
		if !inFields && !inEnums {
			return nil
		}
		return gqlResolveObject(ctx, gqlTypedOf("__Type", name), node.selection, node.hasSel)
	case "__schema":
		return nil
	case "repository":
		owner, _ := args["owner"].(string)
		name, _ := args["name"].(string)
		var repo *RepoRec
		if owner == ViewerLogin && S != nil {
			repo = S.GetRepo(owner, name)
		}
		if repo == nil {
			return nil
		}
		return gqlResolveObject(ctx, gqlTypedOf("Repository", repo), node.selection, node.hasSel)
	case "node":
		id, _ := args["id"].(string)
		kind, numericID, ok := ParseGlobalID(id)
		if !ok {
			return nil
		}
		if kind == "repo" {
			repo := gqlRepoByID(numericID)
			if repo == nil {
				return nil
			}
			return gqlResolveObject(ctx, gqlTypedOf("Repository", repo), node.selection, node.hasSel)
		}
		if kind == "user" {
			return gqlResolveObject(ctx, gqlTypedOf("User", nil), node.selection, node.hasSel)
		}
		if kind == "issue" || kind == "pull" {
			repo, entry := gqlFindEntryByID(numericID)
			if entry == nil {
				return nil
			}
			wrap := &gqlEntryWrap{repo: repo, entry: entry}
			if entry.Kind == "issue" {
				return gqlResolveObject(ctx, gqlTypedOf("Issue", wrap), node.selection, node.hasSel)
			}
			return gqlResolveObject(ctx, gqlTypedOf("PullRequest", wrap), node.selection, node.hasSel)
		}
		return nil
	}
	ctx.errors = append(ctx.errors, fmt.Sprintf("Field '%s' doesn't exist on type 'Query'", field))
	return nil
}

func gqlConnectionOf(ctx *gqlExecCtx, items []gqlTyped, args map[string]interface{}, numberOf func(gqlTyped) int, node *gqlFieldNode, typename string) interface{} {
	first := len(items)
	if f, ok := gqlNum(args["first"]); ok {
		first = int(f)
	}
	startIndex := 0
	if after, ok := args["after"].(string); ok {
		if afterNumber, found := gqlNumberFromCursor(after); found {
			for idx, item := range items {
				if numberOf(item) == afterNumber {
					startIndex = idx + 1
					break
				}
			}
		}
	}
	end := startIndex + first
	if end > len(items) {
		end = len(items)
	}
	if end < startIndex {
		end = startIndex
	}
	page := items[startIndex:end]
	hasNextPage := startIndex+first < len(items)
	var endCursor interface{}
	if len(page) > 0 {
		endCursor = gqlCursorOf(numberOf(page[len(page)-1]))
	}
	connection := gqlTypedOf(typename, &gqlConnVal{
		nodes:    page,
		pageInfo: gqlTypedOf("PageInfo", &gqlPageInfoVal{hasNextPage: hasNextPage, endCursor: endCursor}),
	})
	return gqlResolveObject(ctx, connection, node.selection, node.hasSel)
}

func gqlSortedEntries(repo *RepoRec, kind string) []*EntryRec {
	out := []*EntryRec{}
	for _, entry := range repo.Entries {
		if entry.Kind == kind {
			out = append(out, entry)
		}
	}
	return out
}

func gqlStatesArg(args map[string]interface{}) []string {
	raw, ok := args["states"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		out = append(out, gqlJSString(item))
	}
	return out
}

func gqlContainsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func gqlResolveRepository(ctx *gqlExecCtx, repo *RepoRec, field string, args map[string]interface{}, node *gqlFieldNode) interface{} {
	switch field {
	case "id":
		return GlobalID("repo", repo.ID)
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name
	case "description":
		return repo.Description
	case "isPrivate":
		return repo.IsPrivate
	case "owner":
		return gqlResolveObject(ctx, gqlTypedOf("User", nil), node.selection, node.hasSel)
	case "defaultBranchRef":
		tip := BranchTip(repo, repo.DefaultBranch)
		if tip == "" {
			return nil
		}
		return gqlResolveObject(ctx, gqlTypedOf("Ref", &gqlRefVal{name: repo.DefaultBranch}), node.selection, node.hasSel)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "createdAt":
		return repo.CreatedAt
	case "updatedAt":
		return repo.UpdatedAt
	case "url":
		return ctx.origin + "/" + repo.Owner + "/" + repo.Name
	case "issue":
		number := -1
		if n, ok := gqlNum(args["number"]); ok {
			number = int(n)
		}
		entry := repo.Entries[number]
		if entry == nil || entry.Kind != "issue" {
			return nil
		}
		return gqlResolveObject(ctx, gqlTypedOf("Issue", &gqlEntryWrap{repo: repo, entry: entry}), node.selection, node.hasSel)
	case "issues":
		issues := gqlSortedEntries(repo, "issue")
		if states := gqlStatesArg(args); len(states) > 0 {
			filtered := []*EntryRec{}
			for _, issue := range issues {
				if gqlContainsString(states, gqlIssueState(issue)) {
					filtered = append(filtered, issue)
				}
			}
			issues = filtered
		}
		direction := "DESC"
		if orderBy := asRecord(args["orderBy"]); orderBy != nil {
			if d, ok := orderBy["direction"].(string); ok {
				direction = d
			}
		}
		sort.SliceStable(issues, func(i, j int) bool {
			if direction == "ASC" {
				if issues[i].CreatedSeq != issues[j].CreatedSeq {
					return issues[i].CreatedSeq < issues[j].CreatedSeq
				}
				return issues[i].Number < issues[j].Number
			}
			if issues[i].CreatedSeq != issues[j].CreatedSeq {
				return issues[i].CreatedSeq > issues[j].CreatedSeq
			}
			return issues[i].Number > issues[j].Number
		})
		items := make([]gqlTyped, 0, len(issues))
		for _, issue := range issues {
			items = append(items, gqlTypedOf("Issue", &gqlEntryWrap{repo: repo, entry: issue}))
		}
		return gqlConnectionOf(ctx, items, args, func(item gqlTyped) int {
			return item.value.(*gqlEntryWrap).entry.Number
		}, node, "IssueConnection")
	case "pullRequest":
		number := -1
		if n, ok := gqlNum(args["number"]); ok {
			number = int(n)
		}
		entry := repo.Entries[number]
		if entry == nil || entry.Kind != "pull" {
			return nil
		}
		return gqlResolveObject(ctx, gqlTypedOf("PullRequest", &gqlEntryWrap{repo: repo, entry: entry}), node.selection, node.hasSel)
	case "pullRequests":
		pulls := gqlSortedEntries(repo, "pull")
		if states := gqlStatesArg(args); len(states) > 0 {
			filtered := []*EntryRec{}
			for _, pull := range pulls {
				if gqlContainsString(states, gqlPullState(pull)) {
					filtered = append(filtered, pull)
				}
			}
			pulls = filtered
		}
		sort.SliceStable(pulls, func(i, j int) bool {
			if pulls[i].CreatedSeq != pulls[j].CreatedSeq {
				return pulls[i].CreatedSeq > pulls[j].CreatedSeq
			}
			return pulls[i].Number > pulls[j].Number
		})
		items := make([]gqlTyped, 0, len(pulls))
		for _, pull := range pulls {
			items = append(items, gqlTypedOf("PullRequest", &gqlEntryWrap{repo: repo, entry: pull}))
		}
		return gqlConnectionOf(ctx, items, args, func(item gqlTyped) int {
			return item.value.(*gqlEntryWrap).entry.Number
		}, node, "PullRequestConnection")
	}
	ctx.errors = append(ctx.errors, fmt.Sprintf("Field '%s' doesn't exist on type 'Repository'", field))
	return nil
}

func gqlResolveIssue(ctx *gqlExecCtx, value *gqlEntryWrap, field string, node *gqlFieldNode) interface{} {
	repo, issue := value.repo, value.entry
	switch field {
	case "id":
		return GlobalID("issue", issue.ID)
	case "number":
		return issue.Number
	case "title":
		return issue.Title
	case "body":
		return issue.Body
	case "state":
		return gqlIssueState(issue)
	case "closed":
		return issue.State == "closed"
	case "author":
		return gqlResolveObject(ctx, gqlTypedOf("User", nil), node.selection, node.hasSel)
	case "repository":
		return gqlResolveObject(ctx, gqlTypedOf("Repository", repo), node.selection, node.hasSel)
	case "createdAt":
		return issue.CreatedAt
	case "updatedAt":
		return issue.UpdatedAt
	case "url":
		return fmt.Sprintf("%s/%s/%s/issues/%d", ctx.origin, repo.Owner, repo.Name, issue.Number)
	case "labels":
		nodes := make([]gqlTyped, 0, len(issue.Labels))
		for _, name := range issue.Labels {
			nodes = append(nodes, gqlTypedOf("Label", &gqlLabelVal{name: name}))
		}
		connection := gqlTypedOf("LabelConnection", &gqlConnVal{
			nodes:    nodes,
			pageInfo: gqlTypedOf("PageInfo", &gqlPageInfoVal{hasNextPage: false, endCursor: nil}),
		})
		return gqlResolveObject(ctx, connection, node.selection, node.hasSel)
	case "comments":
		nodes := make([]gqlTyped, 0, len(issue.Comments))
		for i := range issue.Comments {
			nodes = append(nodes, gqlTypedOf("IssueComment", &issue.Comments[i]))
		}
		connection := gqlTypedOf("IssueConnection", &gqlConnVal{
			nodes:    nodes,
			pageInfo: gqlTypedOf("PageInfo", &gqlPageInfoVal{hasNextPage: false, endCursor: nil}),
		})
		return gqlResolveObject(ctx, connection, node.selection, node.hasSel)
	}
	ctx.errors = append(ctx.errors, fmt.Sprintf("Field '%s' doesn't exist on type 'Issue'", field))
	return nil
}

func gqlResolvePull(ctx *gqlExecCtx, value *gqlEntryWrap, field string, args map[string]interface{}, node *gqlFieldNode) interface{} {
	repo, pull := value.repo, value.entry
	switch field {
	case "id":
		return GlobalID("pull", pull.ID)
	case "number":
		return pull.Number
	case "title":
		return pull.Title
	case "body":
		return pull.Body
	case "state":
		return gqlPullState(pull)
	case "merged":
		return pull.Merged
	case "mergedAt":
		if pull.HasMergedAt {
			return pull.MergedAt
		}
		return nil
	case "mergeable":
		return MergeableOf(repo, pull)
	case "headRefName":
		return pull.HeadRef
	case "baseRefName":
		return pull.BaseRef
	case "author":
		return gqlResolveObject(ctx, gqlTypedOf("User", nil), node.selection, node.hasSel)
	case "repository":
		return gqlResolveObject(ctx, gqlTypedOf("Repository", repo), node.selection, node.hasSel)
	case "createdAt":
		return pull.CreatedAt
	case "updatedAt":
		return pull.UpdatedAt
	case "url":
		return fmt.Sprintf("%s/%s/%s/pull/%d", ctx.origin, repo.Owner, repo.Name, pull.Number)
	case "files":
		files := PullFiles(repo, pull)
		items := make([]gqlTyped, 0, len(files))
		for _, file := range files {
			items = append(items, gqlTypedOf("PullRequestChangedFile", &gqlFileVal{
				path: file.Filename, additions: file.Additions, deletions: file.Deletions,
			}))
		}
		return gqlConnectionOf(ctx, items, args, func(gqlTyped) int { return 0 }, node, "PullRequestChangedFileConnection")
	}
	ctx.errors = append(ctx.errors, fmt.Sprintf("Field '%s' doesn't exist on type 'PullRequest'", field))
	return nil
}

// ---------------------------------------------------------------------------
// Mutations

func gqlFindEntryByGlobalID(id string) (*RepoRec, *EntryRec) {
	_, numericID, ok := ParseGlobalID(id)
	if !ok {
		return nil, nil
	}
	return gqlFindEntryByID(numericID)
}

func gqlResolveMutation(ctx *gqlExecCtx, field string, args map[string]interface{}, node *gqlFieldNode) interface{} {
	input := asRecord(args["input"])
	if input == nil {
		input = map[string]interface{}{}
	}
	strArg := func(key string) string {
		if s, ok := input[key].(string); ok {
			return s
		}
		return ""
	}
	switch field {
	case "createIssue":
		repoID := strArg("repositoryId")
		kind, numericID, ok := ParseGlobalID(repoID)
		var repo *RepoRec
		if ok && kind == "repo" {
			repo = gqlRepoByID(numericID)
		}
		if repo == nil {
			ctx.errors = append(ctx.errors, "Repository not found")
			return nil
		}
		issue := CreateIssueRecord(repo, strArg("title"), strArg("body"), []string{})
		return gqlResolveObject(ctx, gqlTypedOf("CreateIssuePayload", &gqlEntryWrap{repo: repo, entry: issue}), node.selection, node.hasSel)
	case "closeIssue":
		var foundRepo *RepoRec
		var foundEntry *EntryRec
		if _, isStr := input["issueId"].(string); isStr {
			foundRepo, foundEntry = gqlFindEntryByGlobalID(strArg("issueId"))
		}
		if foundEntry == nil || foundEntry.Kind != "issue" {
			ctx.errors = append(ctx.errors, "Issue not found")
			return nil
		}
		foundEntry.State = "closed"
		_, iso := S.Tick()
		foundEntry.UpdatedAt = iso
		return gqlResolveObject(ctx, gqlTypedOf("CloseIssuePayload", &gqlEntryWrap{repo: foundRepo, entry: foundEntry}), node.selection, node.hasSel)
	case "createPullRequest":
		repoID := strArg("repositoryId")
		kind, numericID, ok := ParseGlobalID(repoID)
		var repo *RepoRec
		if ok && kind == "repo" {
			repo = gqlRepoByID(numericID)
		}
		if repo == nil {
			ctx.errors = append(ctx.errors, "Repository not found")
			return nil
		}
		pull, errStr := CreatePullRecord(repo, strArg("title"), strArg("body"), strArg("headRefName"), strArg("baseRefName"))
		if errStr != "" {
			ctx.errors = append(ctx.errors, "Could not create pull request: "+errStr)
			return nil
		}
		return gqlResolveObject(ctx, gqlTypedOf("CreatePullRequestPayload", &gqlEntryWrap{repo: repo, entry: pull}), node.selection, node.hasSel)
	case "mergePullRequest":
		var foundRepo *RepoRec
		var foundEntry *EntryRec
		if _, isStr := input["pullRequestId"].(string); isStr {
			foundRepo, foundEntry = gqlFindEntryByGlobalID(strArg("pullRequestId"))
		}
		if foundEntry == nil || foundEntry.Kind != "pull" {
			ctx.errors = append(ctx.errors, "Pull request not found")
			return nil
		}
		_, mergeOK, _ := MergePullRecord(foundRepo, foundEntry)
		if !mergeOK {
			ctx.errors = append(ctx.errors, "Pull request is not mergeable")
		}
		return gqlResolveObject(ctx, gqlTypedOf("MergePullRequestPayload", &gqlEntryWrap{repo: foundRepo, entry: foundEntry}), node.selection, node.hasSel)
	case "addComment":
		var foundEntry *EntryRec
		if _, isStr := input["subjectId"].(string); isStr {
			_, foundEntry = gqlFindEntryByGlobalID(strArg("subjectId"))
		}
		if foundEntry == nil {
			ctx.errors = append(ctx.errors, "Subject not found")
			return nil
		}
		commentID := S.AllocEntityID()
		commentSeq := S.NextSeq()
		_, commentISO := S.Tick()
		foundEntry.Comments = append(foundEntry.Comments, CommentRec{
			ID:          commentID,
			Body:        strArg("body"),
			AuthorLogin: ViewerLogin,
			CreatedSeq:  commentSeq,
			CreatedAt:   commentISO,
		})
		comment := &foundEntry.Comments[len(foundEntry.Comments)-1]
		return gqlResolveObject(ctx, gqlTypedOf("AddCommentPayload", comment), node.selection, node.hasSel)
	}
	ctx.errors = append(ctx.errors, fmt.Sprintf("Field '%s' doesn't exist on type 'Mutation'", field))
	return nil
}

// ---------------------------------------------------------------------------
// Entry point

func gqlRunOperation(doc *gqlDocument, op *gqlOperation, variables map[string]interface{}, origin string) *ojson {
	ctx := &gqlExecCtx{variables: variables, fragments: doc.fragments, errors: []string{}, origin: origin}
	rootType := "Query"
	if op.opType == "mutation" {
		rootType = "Mutation"
	}
	root := gqlTypedOf(rootType, nil)
	data := newOJSON()
	gqlExecSelectionInto(ctx, root, op.selection, data)
	result := newOJSON()
	result.set("data", data)
	if len(ctx.errors) > 0 {
		errs := make([]interface{}, 0, len(ctx.errors))
		for _, msg := range ctx.errors {
			e := newOJSON()
			e.set("message", msg)
			errs = append(errs, e)
		}
		result.set("errors", errs)
	}
	return result
}

func gqlParseDocument(query string) (doc *gqlDocument, errMsg string) {
	defer func() {
		if r := recover(); r != nil {
			doc = nil
			errMsg = fmt.Sprintf("%v", r)
		}
	}()
	parser := &gqlParser{tokens: gqlTokenize(query)}
	doc = parser.parseDocument()
	return doc, ""
}

func gqlRunOne(payload interface{}, origin string) *ojson {
	record := asRecord(payload)
	if record == nil {
		result := newOJSON()
		result.set("data", newOJSON())
		errs := newOJSON()
		errs.set("message", "A GraphQL query string is required")
		result.set("errors", []interface{}{errs})
		return result
	}
	query, isStr := record["query"].(string)
	if !isStr {
		result := newOJSON()
		result.set("data", newOJSON())
		errs := newOJSON()
		errs.set("message", "A GraphQL query string is required")
		result.set("errors", []interface{}{errs})
		return result
	}
	variables := asRecord(record["variables"])
	if variables == nil {
		variables = map[string]interface{}{}
	}
	doc, errMsg := gqlParseDocument(query)
	if errMsg != "" {
		result := newOJSON()
		result.set("data", newOJSON())
		errs := newOJSON()
		errs.set("message", errMsg)
		result.set("errors", []interface{}{errs})
		return result
	}
	if len(doc.operations) == 0 {
		result := newOJSON()
		result.set("data", newOJSON())
		errs := newOJSON()
		errs.set("message", "No operation found")
		result.set("errors", []interface{}{errs})
		return result
	}
	op := &doc.operations[0]
	if opName, isStr := record["operationName"].(string); isStr {
		for i := range doc.operations {
			if doc.operations[i].hasName && doc.operations[i].name == opName {
				op = &doc.operations[i]
				break
			}
		}
	}
	return gqlRunOperation(doc, op, variables, origin)
}

// HandleGraphQL serves POST /api/graphql (single operation or batched array).
func HandleGraphQL(c *Ctx) {
	if c.JSONErr {
		errs := newOJSON()
		errs.set("message", "Problems parsing JSON")
		out := newOJSON()
		out.set("errors", []interface{}{errs})
		sendOrderedJSON(c, 400, out)
		return
	}
	if batch, ok := c.JSON.([]interface{}); ok {
		results := make([]interface{}, 0, len(batch))
		for _, entry := range batch {
			results = append(results, gqlRunOne(entry, c.Origin))
		}
		sendOrderedJSON(c, 200, results)
		return
	}
	sendOrderedJSON(c, 200, gqlRunOne(c.JSON, c.Origin))
}
