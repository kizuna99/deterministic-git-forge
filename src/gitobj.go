package main

// Git object model: hashing, parsing, building, packs, reachability and tree merges.
// Pure functions over an object lookup; the store lives in store.go. Objects are kept as their
// raw payloads (the bytes after the "<type> <len>\0" header) and hashed with SHA-1, exactly
// like git.
//
// Port of entry/gitobj.ts.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// GitObject is a raw git object: Type is "commit" | "tree" | "blob" | "tag".
type GitObject struct {
	Type string
	Data []byte
}

// Lookup resolves an oid to its object, or nil.
type Lookup func(oid string) *GitObject

// Put stores an object and returns its oid.
type Put func(typ string, data []byte) string

// ZeroOID is gitobj.ts ZERO_OID.
const ZeroOID = "0000000000000000000000000000000000000000"

var typeIDs = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
var typeToID = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

// HashObject hashes an object exactly like git: sha1 of "<type> <len>\0" + payload.
func HashObject(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// Sha1Hex is the plain SHA-1 of a byte slice, hex encoded.
func Sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// IsSha40 reports whether s is exactly 40 lowercase hex characters.
func IsSha40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// zlib helpers (sync.Pool based, required for pack throughput)

// zlibWriterPool recycles level-1 (BestSpeed) zlib writers, matching deflateSync level 1.
var zlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return w
	},
}

func deflateData(data []byte) []byte {
	var buf bytes.Buffer
	w := zlibWriterPool.Get().(*zlib.Writer)
	w.Reset(&buf)
	_, _ = w.Write(data)
	_ = w.Close()
	zlibWriterPool.Put(w)
	return buf.Bytes()
}

// zlibReaderPool recycles zlib readers through their Reset method.
var zlibReaderPool = sync.Pool{}

// inflateOnce inflates one zlib stream from the front of slice, reporting how many compressed
// bytes it consumed — a single pass, where the old TS code probed prefix lengths with
// throwing calls.
func inflateOnce(slice []byte) (data []byte, consumed int, err error) {
	br := bytes.NewReader(slice)
	var zr io.ReadCloser
	if pooled := zlibReaderPool.Get(); pooled != nil {
		if rs, ok := pooled.(interface{ Reset(io.Reader) error }); ok {
			if rerr := rs.Reset(br); rerr == nil {
				zr, _ = pooled.(io.ReadCloser)
			}
		}
		if zr == nil {
			// Pooled reader unusable; fall through to a fresh one.
			zr, err = zlib.NewReader(br)
			if err != nil {
				return nil, 0, err
			}
		}
	} else {
		zr, err = zlib.NewReader(br)
		if err != nil {
			return nil, 0, err
		}
	}
	out, rerr := io.ReadAll(zr)
	consumed = len(slice) - br.Len()
	zlibReaderPool.Put(zr)
	if rerr != nil {
		return nil, 0, rerr
	}
	return out, consumed, nil
}

// ---------------------------------------------------------------------------
// Commits, trees, tags

// Identity is one author/committer identity (gitobj.ts Identity).
type Identity struct {
	Name  string
	Email string
	Time  int64
}

// ParsedCommit is gitobj.ts CommitInfo, grouped into identities.
type ParsedCommit struct {
	Tree      string
	Parents   []string
	Author    Identity
	Committer Identity
	Message   string
}

// parseIdent parses "Name <email> 1700000000 +0000" the way the TS parseIdent does.
func parseIdent(line string) Identity {
	lt := strings.Index(line, "<")
	gt := -1
	if lt >= 0 {
		if i := strings.Index(line[lt:], ">"); i >= 0 {
			gt = lt + i
		}
	} else {
		gt = strings.Index(line, ">")
	}
	var name, email, rest string
	if lt >= 0 {
		name = strings.TrimSpace(line[:lt])
	} else {
		name = strings.TrimSpace(line)
	}
	if lt >= 0 && gt > lt {
		email = line[lt+1 : gt]
	}
	if gt >= 0 {
		rest = strings.TrimSpace(line[gt+1:])
	}
	var t int64
	if rest != "" {
		first := strings.Split(rest, " ")[0]
		if n, perr := strconv.ParseInt(first, 10, 64); perr == nil {
			t = n
		}
	}
	return Identity{Name: name, Email: email, Time: t}
}

var commitParseCache sync.Map // map[string]ParsedCommit, keyed by payload bytes
var treeParseCache sync.Map   // map[string][]TreeEntry, keyed by payload bytes

// ParseCommit parses a commit payload.
func ParseCommit(data []byte) ParsedCommit {
	key := string(data)
	if hit, ok := commitParseCache.Load(key); ok {
		return hit.(ParsedCommit)
	}
	info := parseCommitUncached(data)
	commitParseCache.Store(key, info)
	return info
}

func parseCommitUncached(data []byte) ParsedCommit {
	text := string(data)
	header, message := text, ""
	if split := strings.Index(text, "\n\n"); split >= 0 {
		header = text[:split]
		message = text[split+2:]
	}
	info := ParsedCommit{Parents: []string{}, Message: message}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			info.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			info.Parents = append(info.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			info.Author = parseIdent(line[7:])
		case strings.HasPrefix(line, "committer "):
			info.Committer = parseIdent(line[10:])
		}
	}
	return info
}

// BuildCommit serializes a commit exactly like gitobj.ts buildCommit.
func BuildCommit(c ParsedCommit) []byte {
	var b strings.Builder
	b.WriteString("tree " + c.Tree)
	for _, parent := range c.Parents {
		b.WriteString("\nparent " + parent)
	}
	fmt.Fprintf(&b, "\nauthor %s <%s> %d +0000", c.Author.Name, c.Author.Email, c.Author.Time)
	fmt.Fprintf(&b, "\ncommitter %s <%s> %d +0000", c.Committer.Name, c.Committer.Email, c.Committer.Time)
	b.WriteString("\n\n")
	b.WriteString(c.Message)
	return []byte(b.String())
}

// TreeEntry is one entry of a tree object. Mode is kept as its raw string form
// ('100644' | '100755' | '120000' | '40000' — directories are stored without a leading zero
// in the raw bytes).
type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

// ParseTree parses a tree payload.
func ParseTree(data []byte) []TreeEntry {
	key := string(data)
	if hit, ok := treeParseCache.Load(key); ok {
		return hit.([]TreeEntry)
	}
	entries := parseTreeUncached(data)
	treeParseCache.Store(key, entries)
	return entries
}

func parseTreeUncached(data []byte) []TreeEntry {
	entries := []TreeEntry{}
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		space += offset
		mode := string(data[offset:space])
		nulRel := bytes.IndexByte(data[space:], 0x00)
		if nulRel < 0 {
			break
		}
		nul := space + nulRel
		if nul+21 > len(data) {
			break
		}
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, Oid: oid})
		offset = nul + 21
	}
	return entries
}

// CompareTreeEntries implements git's tree ordering: names compare bytewise, with
// directories sorting as if they ended in '/'.
func CompareTreeEntries(a, b TreeEntry) int {
	an, bn := a.Name, b.Name
	if a.Mode == "40000" {
		an += "/"
	}
	if b.Mode == "40000" {
		bn += "/"
	}
	switch {
	case an < bn:
		return -1
	case an > bn:
		return 1
	default:
		return 0
	}
}

// BuildTree serializes entries (sorted in git tree order) into a tree payload.
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return CompareTreeEntries(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, entry := range sorted {
		buf.WriteString(entry.Mode + " " + entry.Name + "\x00")
		if raw, err := hex.DecodeString(entry.Oid); err == nil {
			buf.Write(raw)
		}
	}
	return buf.Bytes()
}

// ParseTagTarget extracts the target oid from a tag payload (the ^object <sha> line), or "".
func ParseTagTarget(data []byte) string {
	text := string(data)
	for len(text) > 0 {
		line := text
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			line, text = text[:i], text[i+1:]
		} else {
			text = ""
		}
		if strings.HasPrefix(line, "object ") && len(line) >= 47 {
			candidate := line[7:47]
			if IsSha40(candidate) {
				return candidate
			}
		}
	}
	return ""
}

// PeelToCommit peels a tag (or any object) down to the commit it ultimately points at;
// "" if it never lands on one.
func PeelToCommit(l Lookup, oid string) string {
	return peelToCommit(l, oid, 0)
}

func peelToCommit(l Lookup, oid string, depth int) string {
	if depth > 16 {
		return ""
	}
	obj := l(oid)
	if obj == nil {
		return ""
	}
	if obj.Type == "commit" {
		return oid
	}
	if obj.Type == "tag" {
		target := ParseTagTarget(obj.Data)
		if target != "" {
			return peelToCommit(l, target, depth+1)
		}
		return ""
	}
	return ""
}

// ---------------------------------------------------------------------------
// Reachability and ancestry

// ordSet is an insertion-ordered string set, mirroring JS Set iteration order. Pack byte
// order depends on it, so reachability results that feed pack building keep their order.
type ordSet struct {
	seen  map[string]bool
	order []string
}

func newOrdSet() *ordSet { return &ordSet{seen: make(map[string]bool)} }

func (s *ordSet) add(v string) bool {
	if s.seen[v] {
		return false
	}
	s.seen[v] = true
	s.order = append(s.order, v)
	return true
}

func (s *ordSet) has(v string) bool { return s.seen[v] }

// collectReachableOrdered is collectReachable keeping the TS Set insertion order: a LIFO
// stack seeded with the roots in order; a commit pushes its tree then its parents in order,
// a tree pushes its entries in order, a tag pushes its target.
func collectReachableOrdered(l Lookup, roots []string) []string {
	set := newOrdSet()
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if set.has(oid) {
			continue
		}
		obj := l(oid)
		if obj == nil {
			continue
		}
		set.add(oid)
		switch obj.Type {
		case "commit":
			commit := ParseCommit(obj.Data)
			if commit.Tree != "" {
				stack = append(stack, commit.Tree)
			}
			stack = append(stack, commit.Parents...)
		case "tree":
			for _, entry := range ParseTree(obj.Data) {
				stack = append(stack, entry.Oid)
			}
		case "tag":
			if target := ParseTagTarget(obj.Data); target != "" {
				stack = append(stack, target)
			}
		}
	}
	return set.order
}

// CollectReachable returns every object reachable from the given roots (commits walk to
// trees, trees to entries, tags to targets).
func CollectReachable(l Lookup, roots []string) map[string]bool {
	out := make(map[string]bool)
	for _, oid := range collectReachableOrdered(l, roots) {
		out[oid] = true
	}
	return out
}

// CommitAncestors returns all commit ancestors of a commit, itself included.
func CommitAncestors(l Lookup, tip string) map[string]bool {
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := l(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		seen[oid] = true
		stack = append(stack, ParseCommit(obj.Data).Parents...)
	}
	return seen
}

// IsAncestor reports whether ancestor is reachable from descendant by parent walks
// (a commit is its own ancestor).
func IsAncestor(l Lookup, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := l(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		stack = append(stack, ParseCommit(obj.Data).Parents...)
	}
	return false
}

// MergeBase returns the best common ancestor of two commits (first shared commit on a
// breadth-first walk from b), or "" if there is none.
func MergeBase(l Lookup, a, b string) string {
	ancestorsOfA := CommitAncestors(l, a)
	seen := make(map[string]bool)
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestorsOfA[oid] {
			return oid
		}
		obj := l(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		queue = append(queue, ParseCommit(obj.Data).Parents...)
	}
	return ""
}

// ShallowResult is the result of CollectShallow: the objects to send (as a set and in TS
// Set insertion order) and the new shallow boundary.
type ShallowResult struct {
	Objects      map[string]bool
	ObjectsOrder []string
	Boundary     []string
}

// CollectShallow is depth-limited reachability for shallow fetches. Commits at exactly
// depth generations from a want (the want itself is generation 1) that still have parents
// form the boundary; ancestors beyond the boundary are excluded entirely.
func CollectShallow(l Lookup, wants []string, depth int) ShallowResult {
	type queueItem struct {
		oid   string
		depth int
	}
	commitDepth := make(map[string]int)
	var commitOrder []string
	var queue []queueItem
	for _, want := range wants {
		if commit := PeelToCommit(l, want); commit != "" {
			queue = append(queue, queueItem{commit, 1})
		}
	}
	var boundary []string
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if known, ok := commitDepth[item.oid]; ok && known <= item.depth {
			continue
		}
		if _, ok := commitDepth[item.oid]; !ok {
			commitOrder = append(commitOrder, item.oid)
		}
		commitDepth[item.oid] = item.depth
		obj := l(item.oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		commit := ParseCommit(obj.Data)
		if item.depth >= depth {
			if len(commit.Parents) > 0 {
				boundary = append(boundary, item.oid)
			}
			continue
		}
		for _, parent := range commit.Parents {
			queue = append(queue, queueItem{parent, item.depth + 1})
		}
	}
	objects := newOrdSet()
	for _, oid := range commitOrder {
		objects.add(oid)
		obj := l(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		tree := ParseCommit(obj.Data).Tree
		if tree != "" {
			// Full tree closure for every included commit, minus nothing: trees are
			// shared freely.
			stack := []string{tree}
			for len(stack) > 0 {
				current := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if objects.has(current) {
					continue
				}
				treeObj := l(current)
				if treeObj == nil {
					continue
				}
				objects.add(current)
				if treeObj.Type == "tree" {
					for _, entry := range ParseTree(treeObj.Data) {
						stack = append(stack, entry.Oid)
					}
				}
			}
		}
	}
	// Tag objects pointing at wanted commits ride along (the client asks for the ref
	// target itself).
	for _, want := range wants {
		if obj := l(want); obj != nil && obj.Type == "tag" {
			objects.add(want)
		}
	}
	boundarySet := newOrdSet()
	for _, oid := range boundary {
		boundarySet.add(oid)
	}
	return ShallowResult{
		Objects:      objects.seen,
		ObjectsOrder: objects.order,
		Boundary:     boundarySet.order,
	}
}

// ---------------------------------------------------------------------------
// Tree flattening, path access, rebuild — the contents API's working set

// FlatEntry is one flattened file: Mode is the decimal form of the git mode string
// (100644, 100755, 120000) and Oid the blob oid.
type FlatEntry struct {
	Mode int
	Oid  string
}

func modeStringToInt(mode string) int {
	n, err := strconv.Atoi(mode)
	if err != nil {
		return 0
	}
	return n
}

// FlattenTree flattens a tree into path -> FlatEntry for files (and symlinks), recursing
// into subtrees.
func FlattenTree(l Lookup, treeOid string) map[string]FlatEntry {
	out := make(map[string]FlatEntry)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := l(oid)
		if obj == nil || obj.Type != "tree" {
			return
		}
		for _, entry := range ParseTree(obj.Data) {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.Oid, path)
			} else {
				out[path] = FlatEntry{Mode: modeStringToInt(entry.Mode), Oid: entry.Oid}
			}
		}
	}
	if treeOid != "" {
		walk(treeOid, "")
	}
	return out
}

// PathLookup is the result of LookupPath.
type PathLookup struct {
	Kind string // "blob" | "tree"
	Oid  string
	Mode string
}

// LookupPath looks up one path inside a tree; ok is false when the path does not resolve.
func LookupPath(l Lookup, treeOid, path string) (PathLookup, bool) {
	var parts []string
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	current := treeOid
	for i, part := range parts {
		obj := l(current)
		if obj == nil || obj.Type != "tree" {
			return PathLookup{}, false
		}
		var found *TreeEntry
		for _, candidate := range ParseTree(obj.Data) {
			if candidate.Name == part {
				c := candidate
				found = &c
				break
			}
		}
		if found == nil {
			return PathLookup{}, false
		}
		if i == len(parts)-1 {
			if found.Mode == "40000" {
				return PathLookup{Kind: "tree", Oid: found.Oid, Mode: found.Mode}, true
			}
			return PathLookup{Kind: "blob", Oid: found.Oid, Mode: found.Mode}, true
		}
		if found.Mode != "40000" {
			return PathLookup{}, false
		}
		current = found.Oid
	}
	return PathLookup{}, false
}

// ListTreeLevel lists one directory level of a tree (empty for non-trees).
func ListTreeLevel(l Lookup, treeOid string) []TreeEntry {
	obj := l(treeOid)
	if obj == nil || obj.Type != "tree" {
		return []TreeEntry{}
	}
	return ParseTree(obj.Data)
}

// BuildTreeFromFlat rebuilds a tree hierarchy from a flattened file map, storing every new
// tree object through p. Returns the root tree oid (the empty tree when the map is empty).
func BuildTreeFromFlat(flat map[string]FlatEntry, p Put) string {
	type dir struct {
		files map[string]FlatEntry
		dirs  map[string]*dir
	}
	newDir := func() *dir {
		return &dir{files: make(map[string]FlatEntry), dirs: make(map[string]*dir)}
	}
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
		entries := []TreeEntry{}
		for name, file := range d.files {
			entries = append(entries, TreeEntry{Mode: strconv.Itoa(file.Mode), Name: name, Oid: file.Oid})
		}
		for name, sub := range d.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: emit(sub)})
		}
		return p("tree", BuildTree(entries))
	}
	return emit(root)
}

// ---------------------------------------------------------------------------
// Three-way tree merge (squash merges and mergeability)

// MergeOutcome is the result of MergeTrees.
type MergeOutcome struct {
	Tree     string
	Conflict bool
}

// MergeTrees merges theirs into ours relative to base, tree by tree. A path changed on
// only one side takes that side; changed identically on both takes it; changed differently
// on both (including delete-vs-modify and file-vs-directory) is a conflict. No line-level
// merging.
func MergeTrees(l Lookup, p Put, base, ours, theirs string) MergeOutcome {
	if ours == theirs {
		return MergeOutcome{Tree: ours, Conflict: false}
	}
	if base == ours {
		return MergeOutcome{Tree: theirs, Conflict: false}
	}
	if base == theirs {
		return MergeOutcome{Tree: ours, Conflict: false}
	}

	index := func(entries []TreeEntry) (map[string]TreeEntry, []string) {
		m := make(map[string]TreeEntry, len(entries))
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if _, dup := m[e.Name]; !dup {
				names = append(names, e.Name)
			}
			m[e.Name] = e
		}
		return m, names
	}
	baseEntries, baseNames := index(ListTreeLevel(l, base))
	ourEntries, ourNames := index(ListTreeLevel(l, ours))
	theirEntries, theirNames := index(ListTreeLevel(l, theirs))

	// Name union in TS Set insertion order: base keys, then ours, then theirs.
	nameSet := newOrdSet()
	for _, n := range baseNames {
		nameSet.add(n)
	}
	for _, n := range ourNames {
		nameSet.add(n)
	}
	for _, n := range theirNames {
		nameSet.add(n)
	}

	sameEntry := func(a TreeEntry, aOk bool, b TreeEntry, bOk bool) bool {
		if !aOk || !bOk {
			return aOk == bOk
		}
		return a.Mode == b.Mode && a.Oid == b.Oid
	}

	merged := []TreeEntry{}
	conflict := false
	for _, name := range nameSet.order {
		b, bOk := baseEntries[name]
		o, oOk := ourEntries[name]
		t, tOk := theirEntries[name]
		if sameEntry(o, oOk, t, tOk) {
			if oOk {
				merged = append(merged, o)
			}
			continue
		}
		if sameEntry(b, bOk, o, oOk) {
			if tOk {
				merged = append(merged, t)
			}
			continue
		}
		if sameEntry(b, bOk, t, tOk) {
			if oOk {
				merged = append(merged, o)
			}
			continue
		}
		// Both sides changed this name differently.
		if oOk && tOk && o.Mode == "40000" && t.Mode == "40000" {
			subBase := ""
			if bOk && b.Mode == "40000" {
				subBase = b.Oid
			}
			sub := MergeTrees(l, p, subBase, o.Oid, t.Oid)
			if sub.Conflict {
				conflict = true
			}
			merged = append(merged, TreeEntry{Mode: "40000", Name: name, Oid: sub.Tree})
			continue
		}
		conflict = true
		if oOk {
			merged = append(merged, o)
		} else if tOk {
			merged = append(merged, t)
		}
	}
	return MergeOutcome{Tree: p("tree", BuildTree(merged)), Conflict: conflict}
}

// ---------------------------------------------------------------------------
// Line diff counts for the pull-request files view

// LineDiffCounts counts additions/deletions between two text payloads via a simple LCS
// over lines (with the TS prefix/suffix fallback for very large inputs).
func LineDiffCounts(oldText, newText string) (additions, deletions int) {
	splitLines := func(text string) []string {
		if text == "" {
			return []string{}
		}
		lines := strings.Split(text, "\n")
		// Trailing newline produces a phantom empty final element; drop it.
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)
	m, n := len(oldLines), len(newLines)
	if int64(m)*int64(n) > 4_000_000 {
		// Very large files: fall back to a prefix/suffix trim count rather than an
		// O(mn) table.
		prefix := 0
		for prefix < m && prefix < n && oldLines[prefix] == newLines[prefix] {
			prefix++
		}
		suffix := 0
		for suffix < m-prefix && suffix < n-prefix && oldLines[m-1-suffix] == newLines[n-1-suffix] {
			suffix++
		}
		return n - prefix - suffix, m - prefix - suffix
	}
	dp := make([]int, (m+1)*(n+1))
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i*(n+1)+j] = dp[(i+1)*(n+1)+j+1] + 1
			} else if dp[(i+1)*(n+1)+j] >= dp[i*(n+1)+j+1] {
				dp[i*(n+1)+j] = dp[(i+1)*(n+1)+j]
			} else {
				dp[i*(n+1)+j] = dp[i*(n+1)+j+1]
			}
		}
	}
	common := dp[0]
	return n - common, m - common
}

// ---------------------------------------------------------------------------
// Packs

// PackObject is one object in a pack (also the input element for WritePack).
type PackObject struct {
	Oid  string
	Type string
	Data []byte
}

// WritePack serializes objects into a pack v2 byte stream (plain objects only,
// zlib-deflated whole).
func WritePack(objects []PackObject) []byte {
	var body bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	header[4], header[5], header[6], header[7] = 0, 0, 0, 2
	count := len(objects)
	header[8] = byte(count >> 24)
	header[9] = byte(count >> 16)
	header[10] = byte(count >> 8)
	header[11] = byte(count)
	body.Write(header)
	for _, obj := range objects {
		size := len(obj.Data)
		first := (typeToID[obj.Type] << 4) | (size & 0x0f)
		rest := size >> 4
		headBytes := []byte{}
		if rest > 0 {
			first |= 0x80
		}
		headBytes = append(headBytes, byte(first))
		for rest > 0 {
			b := byte(rest & 0x7f)
			rest >>= 7
			if rest > 0 {
				b |= 0x80
			}
			headBytes = append(headBytes, b)
		}
		body.Write(headBytes)
		body.Write(deflateData(obj.Data))
	}
	trailer := sha1.Sum(body.Bytes())
	out := make([]byte, 0, body.Len()+len(trailer))
	out = append(out, body.Bytes()...)
	out = append(out, trailer[:]...)
	return out
}

// ReadPack parses a pack, resolving OFS/REF deltas against objects earlier in the pack or
// through base (the receiver's existing store, for thin packs). It returns an error on any
// malformed input (the TS throws; the messages match).
func ReadPack(pack []byte, base Lookup) ([]PackObject, error) {
	if len(pack) < 12+20 {
		return nil, errors.New("pack too short")
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("bad pack signature")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	dataEnd := len(pack) - 20
	computed := sha1.Sum(pack[:dataEnd])
	if !bytes.Equal(computed[:], pack[dataEnd:]) {
		return nil, errors.New("pack checksum mismatch")
	}

	byOffset := make(map[int]PackObject)
	byOid := make(map[string]PackObject)
	var oidOrder []string
	offset := 12

	readVarintOffset := func() (int, error) {
		if offset >= dataEnd {
			return 0, errors.New("pack truncated")
		}
		b := pack[offset]
		offset++
		value := int(b & 0x7f)
		for b&0x80 != 0 {
			if offset >= dataEnd {
				return 0, errors.New("pack truncated")
			}
			b = pack[offset]
			offset++
			value = ((value + 1) << 7) | int(b&0x7f)
		}
		return value, nil
	}

	for i := 0; i < count; i++ {
		if offset >= dataEnd {
			return nil, errors.New("pack truncated")
		}
		objectStart := offset
		b := pack[offset]
		offset++
		typeID := (b >> 4) & 0x07
		size := int(b & 0x0f)
		shift := 4
		for b&0x80 != 0 {
			if offset >= dataEnd {
				return nil, errors.New("pack truncated")
			}
			b = pack[offset]
			offset++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		var resolved PackObject
		switch typeID {
		case 6: // OFS_DELTA
			back, err := readVarintOffset()
			if err != nil {
				return nil, err
			}
			baseObj, ok := byOffset[objectStart-back]
			if !ok {
				return nil, errors.New("ofs-delta base missing")
			}
			inflated, consumed, err := inflateOnce(pack[offset:dataEnd])
			if err != nil {
				return nil, err
			}
			offset += consumed
			data, err := ApplyDelta(baseObj.Data, inflated)
			if err != nil {
				return nil, err
			}
			resolved = PackObject{Type: baseObj.Type, Data: data}
		case 7: // REF_DELTA
			if offset+20 > dataEnd {
				return nil, errors.New("pack truncated")
			}
			baseOID := hex.EncodeToString(pack[offset : offset+20])
			offset += 20
			var stored *PackObject
			if inPack, ok := byOid[baseOID]; ok {
				p := inPack
				stored = &p
			} else if baseObj := base(baseOID); baseObj != nil {
				stored = &PackObject{Type: baseObj.Type, Data: baseObj.Data}
			}
			if stored == nil {
				return nil, errors.New("ref-delta base missing")
			}
			inflated, consumed, err := inflateOnce(pack[offset:dataEnd])
			if err != nil {
				return nil, err
			}
			offset += consumed
			data, err := ApplyDelta(stored.Data, inflated)
			if err != nil {
				return nil, err
			}
			resolved = PackObject{Type: stored.Type, Data: data}
		default:
			typ, ok := typeIDs[int(typeID)]
			if !ok {
				return nil, fmt.Errorf("unknown pack object type %d", typeID)
			}
			inflated, consumed, err := inflateOnce(pack[offset:dataEnd])
			if err != nil {
				return nil, err
			}
			offset += consumed
			if len(inflated) != size {
				return nil, errors.New("pack object size mismatch")
			}
			resolved = PackObject{Type: typ, Data: inflated}
		}
		resolved.Oid = HashObject(resolved.Type, resolved.Data)
		byOffset[objectStart] = resolved
		if _, exists := byOid[resolved.Oid]; !exists {
			oidOrder = append(oidOrder, resolved.Oid)
		}
		byOid[resolved.Oid] = resolved
	}
	out := make([]PackObject, 0, len(oidOrder))
	for _, oid := range oidOrder {
		out = append(out, byOid[oid])
	}
	return out, nil
}

// ApplyDelta applies a git delta buffer to a base payload.
func ApplyDelta(baseData, delta []byte) ([]byte, error) {
	offset := 0
	readVarint := func() (int, error) {
		value := 0
		shift := 0
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[offset]
			offset++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(baseData) {
		return nil, errors.New("delta base size mismatch")
	}
	targetSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	produced := 0
	for offset < len(delta) {
		opcode := delta[offset]
		offset++
		if opcode&0x80 != 0 {
			copyOffset, copySize := 0, 0
			for bit := 0; bit < 4; bit++ {
				if opcode&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					copyOffset |= int(delta[offset]) << (8 * bit)
					offset++
				}
			}
			for bit := 0; bit < 3; bit++ {
				if opcode&(0x10<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					copySize |= int(delta[offset]) << (8 * bit)
					offset++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(baseData) {
				return nil, errors.New("delta copy out of range")
			}
			out.Write(baseData[copyOffset : copyOffset+copySize])
			produced += copySize
		} else if opcode > 0 {
			if offset+int(opcode) > len(delta) {
				return nil, errors.New("delta truncated")
			}
			out.Write(delta[offset : offset+int(opcode)])
			produced += int(opcode)
			offset += int(opcode)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	result := out.Bytes()
	if len(result) != targetSize || produced != targetSize {
		return nil, errors.New("delta target size mismatch")
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Pkt-line framing

// PktLine frames a text payload as a pkt-line. It panics if the framed length would exceed
// 0xffff, mirroring the TS throw.
func PktLine(payload string) []byte {
	return PktLineBytes([]byte(payload))
}

// PktLineBytes frames a binary payload as a pkt-line.
func PktLineBytes(body []byte) []byte {
	length := len(body) + 4
	if length > 0xffff {
		panic("pkt-line too large")
	}
	out := make([]byte, 0, length)
	out = append(out, []byte(fmt.Sprintf("%04x", length))...)
	out = append(out, body...)
	return out
}

// FlushPkt and DelimPkt are the pkt-line control packets (gitobj.ts FLUSH_PKT / DELIM_PKT).
var FlushPkt = []byte("0000")
var DelimPkt = []byte("0001")

// PktRead is the result of ReadPktLines: each payload in order, with the flush packet (and
// the v2 response-end packet) represented as a nil entry and the delimiter packet as a
// one-byte entry {0x01}; Rest is the byte offset just past the last consumed packet.
type PktRead struct {
	Lines [][]byte
	Rest  int
}

func isHex4(b []byte) bool {
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ReadPktLines parses pkt-lines from the front of buffer starting at start. A flush (or
// response-end) packet terminates the segment. It returns an error when a length prefix is
// malformed (the TS throws; messages match).
func ReadPktLines(buffer []byte, start int) (PktRead, error) {
	lines := [][]byte{}
	offset := start
	for offset < len(buffer) {
		if offset+4 > len(buffer) {
			return PktRead{}, errors.New("truncated pkt-line")
		}
		hexPart := buffer[offset : offset+4]
		if !isHex4(hexPart) {
			return PktRead{}, errors.New("invalid pkt-line length")
		}
		length64, _ := strconv.ParseInt(string(hexPart), 16, 32)
		length := int(length64)
		switch length {
		case 0:
			lines = append(lines, nil)
			offset += 4
			return PktRead{Lines: lines, Rest: offset}, nil
		case 1:
			lines = append(lines, []byte{0x01})
			offset += 4
			continue
		case 2:
			// response-end packet (v2): treat as a segment terminator like flush
			lines = append(lines, nil)
			offset += 4
			return PktRead{Lines: lines, Rest: offset}, nil
		}
		if length < 4 || offset+length > len(buffer) {
			return PktRead{}, errors.New("pkt-line overruns buffer")
		}
		payload := make([]byte, length-4)
		copy(payload, buffer[offset+4:offset+length])
		lines = append(lines, payload)
		offset += length
		if len(lines) > 100000 {
			return PktRead{}, errors.New("too many pkt-lines")
		}
	}
	return PktRead{Lines: lines, Rest: offset}, nil
}

// PktSegment is the result of ReadPktSegment.
type PktSegment struct {
	Lines [][]byte
	Rest  int
}

// ReadPktSegment reads one pkt-line segment starting at start: payloads up to the
// terminating flush (or response-end) packet. Delimiter packets are kept as a one-byte
// line {0x01} so callers can split command arguments. Rest is the offset just past the
// terminator — the start of the next segment, or of raw pack bytes after a push's command
// block.
func ReadPktSegment(buffer []byte, start int) (PktSegment, error) {
	lines := [][]byte{}
	offset := start
	for offset < len(buffer) {
		if offset+4 > len(buffer) {
			return PktSegment{}, errors.New("truncated pkt-line")
		}
		hexPart := buffer[offset : offset+4]
		if !isHex4(hexPart) {
			return PktSegment{}, errors.New("invalid pkt-line length")
		}
		length64, _ := strconv.ParseInt(string(hexPart), 16, 32)
		length := int(length64)
		if length == 0 || length == 2 {
			return PktSegment{Lines: lines, Rest: offset + 4}, nil
		}
		if length == 1 {
			lines = append(lines, []byte{0x01})
			offset += 4
			continue
		}
		if length < 4 || offset+length > len(buffer) {
			return PktSegment{}, errors.New("pkt-line overruns buffer")
		}
		payload := make([]byte, length-4)
		copy(payload, buffer[offset+4:offset+length])
		lines = append(lines, payload)
		offset += length
		if len(lines) > 100000 {
			return PktSegment{}, errors.New("too many pkt-lines")
		}
	}
	return PktSegment{Lines: lines, Rest: offset}, nil
}
