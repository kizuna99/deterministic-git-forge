package main

// Git smart HTTP: info/refs advertisements (protocol v0 and v2), upload-pack (ls-refs and
// fetch, shallow included) and receive-pack (push with per-ref reports). Plain objects only
// in packs the server builds; full delta resolution for packs it receives.
//
// Port of entry/gitserver.ts. The Sender type is defined in app.go:
//
//	type Sender func(status int, contentType string, body []byte)

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const gitAgent = "forge/1.0.0"
const uploadCapsV0 = "side-band-64k thin-pack ofs-delta shallow multi_ack_detailed symref=HEAD:refs/heads/main agent=" + gitAgent
const receiveCaps = "report-status side-band-64k delete-refs ofs-delta quiet agent=" + gitAgent

// Advertisement and ls-refs bytes are pure functions of the ref state, so they are memoized
// per repository against refsVersion: the clone benchmark asks for the same bytes 150 times.
// (The TS uses WeakMap keyed by the repo record; a plain map keyed by pointer is the Go
// equivalent for the life of the process.)
var (
	advertCachesMu sync.Mutex
	advertCaches   = make(map[*RepoRec]map[string][]byte)
)

func cachedBytes(repo *RepoRec, key string, build func() []byte) []byte {
	advertCachesMu.Lock()
	perRepo, ok := advertCaches[repo]
	if !ok {
		perRepo = make(map[string][]byte)
		advertCaches[repo] = perRepo
	}
	advertCachesMu.Unlock()
	if hit, ok := perRepo[key]; ok {
		return hit
	}
	built := build()
	advertCachesMu.Lock()
	if len(perRepo) >= 64 {
		for k := range perRepo {
			delete(perRepo, k)
		}
	}
	perRepo[key] = built
	advertCachesMu.Unlock()
	return built
}

func sortedRefs(repo *RepoRec) [][2]string {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, name := range names {
		out = append(out, [2]string{name, repo.Refs[name]})
	}
	return out
}

func headOid(repo *RepoRec) string {
	if tip := BranchTip(repo, repo.DefaultBranch); tip != "" {
		return tip
	}
	return ZeroOID
}

func sendText(send Sender, status int, contentType, body string) {
	send(status, contentType, []byte(body))
}

// ---------------------------------------------------------------------------
// Advertisements

// InfoRefs serves the info/refs advertisement for git-upload-pack / git-receive-pack,
// protocol v0 or v2.
func InfoRefs(repo *RepoRec, service string, protocolV2 bool, send Sender) {
	if service != "git-upload-pack" && service != "git-receive-pack" {
		sendText(send, 403, "text/plain", "unknown service\n")
		return
	}
	if repo == nil {
		sendText(send, 404, "text/plain", "repository not found\n")
		return
	}
	build := func() []byte {
		var buf bytes.Buffer
		buf.Write(PktLine("# service=" + service + "\n"))
		buf.Write(FlushPkt)
		switch {
		case service == "git-upload-pack" && protocolV2:
			buf.Write(PktLine("version 2"))
			buf.Write(PktLine("agent=" + gitAgent))
			buf.Write(PktLine("ls-refs=unborn"))
			buf.Write(PktLine("fetch=shallow wait-for-done"))
			buf.Write(PktLine("object-format=sha1"))
			buf.Write(FlushPkt)
		case service == "git-upload-pack":
			refs := sortedRefs(repo)
			if len(refs) == 0 {
				buf.Write(PktLine(ZeroOID + " capabilities^{}\x00" + uploadCapsV0))
			} else {
				buf.Write(PktLine(headOid(repo) + " HEAD\x00" + uploadCapsV0))
				for _, ref := range refs {
					buf.Write(PktLine(ref[1] + " " + ref[0]))
				}
			}
			buf.Write(FlushPkt)
		default:
			// git-receive-pack: always the v0 advertisement.
			refs := sortedRefs(repo)
			if len(refs) == 0 {
				buf.Write(PktLine(ZeroOID + " capabilities^{}\x00" + receiveCaps))
			} else {
				for i, ref := range refs {
					if i == 0 {
						buf.Write(PktLine(ref[1] + " " + ref[0] + "\x00" + receiveCaps))
					} else {
						buf.Write(PktLine(ref[1] + " " + ref[0]))
					}
				}
			}
			buf.Write(FlushPkt)
		}
		return buf.Bytes()
	}
	v := "v0"
	if protocolV2 {
		v = "v2"
	}
	cacheKey := "adv|" + service + "|" + v + "|" + strconv.Itoa(repo.RefsVersion)
	send(200, "application/x-"+service+"-advertisement", cachedBytes(repo, cacheKey, build))
}

// ---------------------------------------------------------------------------
// upload-pack

func lsRefs(repo *RepoRec, args []string, send Sender) {
	cacheKey := "ls|" + strconv.Itoa(repo.RefsVersion) + "|" + strings.Join(args, "\x01")
	body := cachedBytes(repo, cacheKey, func() []byte { return buildLsRefs(repo, args) })
	send(200, "application/x-git-upload-pack-result", body)
}

func buildLsRefs(repo *RepoRec, args []string) []byte {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, arg := range args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case arg == "unborn":
			unborn = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, arg[len("ref-prefix "):])
		}
	}
	lookup := RepoLookup(repo)
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var buf bytes.Buffer
	headRef := "refs/heads/" + repo.DefaultBranch
	if headTarget, ok := repo.Refs[headRef]; ok {
		if matches("HEAD") {
			line := headTarget + " HEAD"
			if symrefs {
				line += " symref-target:" + headRef
			}
			buf.Write(PktLine(line))
		}
	} else if unborn && matches("HEAD") {
		buf.Write(PktLine("unborn HEAD symref-target:" + headRef))
	}
	for _, ref := range sortedRefs(repo) {
		name, oid := ref[0], ref[1]
		if !matches(name) {
			continue
		}
		line := oid + " " + name
		if peel {
			if obj := lookup(oid); obj != nil && obj.Type == "tag" {
				// peel to the commit for the peeled: annotation
				target := oid
				for guard := 0; guard < 16; {
					guard++
					current := lookup(target)
					if current == nil || current.Type != "tag" {
						break
					}
					next := ParseTagTarget(current.Data)
					if next == "" {
						break
					}
					target = next
				}
				if targetObj := lookup(target); targetObj != nil && targetObj.Type == "commit" {
					line += " peeled:" + target
				}
			}
		}
		buf.Write(PktLine(line))
	}
	buf.Write(FlushPkt)
	return buf.Bytes()
}

// fetchPlan is a computed fetch: the pack, its sideband framing, and the negotiation lines.
type fetchPlan struct {
	pack           []byte
	framed         []byte
	shallowLines   []string
	unshallowLines []string
	commonHaves    []string
	hasHaves       bool
}

// Per-repo memo of whole fetch plans: the clone benchmark asks for the same tip closure
// fifty times over, and a plan is a pure function of (wants, haves, shallows, depth, head)
// over immutable objects — so a computed plan stays valid for the life of the repository
// record.
var (
	planCachesMu sync.Mutex
	planCaches   = make(map[*RepoRec]map[string]*fetchPlan)
)

const planCacheMax = 128

type fetchOpts struct {
	wants         []string
	haves         []string
	clientShallow []string
	depth         *int
	deepenSince   *int
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func planFetch(repo *RepoRec, opts fetchOpts) *fetchPlan {
	lookup := RepoLookup(repo)
	depthKey, deepenSinceKey := "", ""
	if opts.depth != nil {
		depthKey = strconv.Itoa(*opts.depth)
	}
	if opts.deepenSince != nil {
		deepenSinceKey = strconv.Itoa(*opts.deepenSince)
	}
	cacheKey := strings.Join([]string{
		repo.Owner,
		repo.Name,
		strings.Join(sortedCopy(opts.wants), ","),
		strings.Join(sortedCopy(opts.haves), ","),
		strings.Join(sortedCopy(opts.clientShallow), ","),
		depthKey,
		deepenSinceKey,
		headOid(repo),
	}, "|")
	planCachesMu.Lock()
	perRepo, ok := planCaches[repo]
	if !ok {
		perRepo = make(map[string]*fetchPlan)
		planCaches[repo] = perRepo
	}
	planCachesMu.Unlock()
	if cached, ok := perRepo[cacheKey]; ok {
		return cached
	}

	// What the client already holds: the closure of its haves, except that a commit the
	// client marks shallow is grafted — its parents are NOT held, so history below the
	// boundary must be sendable when a deepen asks for it.
	haveSet := make(map[string]bool)
	if len(opts.haves) > 0 {
		shallowSet := make(map[string]bool, len(opts.clientShallow))
		for _, oid := range opts.clientShallow {
			shallowSet[oid] = true
		}
		stack := append([]string(nil), opts.haves...)
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if haveSet[oid] {
				continue
			}
			obj := lookup(oid)
			if obj == nil {
				continue
			}
			haveSet[oid] = true
			switch obj.Type {
			case "commit":
				commit := ParseCommit(obj.Data)
				if commit.Tree != "" {
					stack = append(stack, commit.Tree)
				}
				if !shallowSet[oid] {
					stack = append(stack, commit.Parents...)
				}
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
	}
	commonHaves := []string{}
	for _, oid := range opts.haves {
		if lookup(oid) != nil {
			commonHaves = append(commonHaves, oid)
		}
	}

	var includeOrder []string
	includeSet := make(map[string]bool)
	boundary := []string{}
	switch {
	case opts.depth != nil:
		shallow := CollectShallow(lookup, opts.wants, *opts.depth)
		includeOrder = shallow.ObjectsOrder
		includeSet = shallow.Objects
		boundary = shallow.Boundary
	case opts.deepenSince != nil:
		// Depth-by-time: include commits newer than the cutoff, boundary at the first
		// older parents.
		include := newOrdSet()
		stack := append([]string(nil), opts.wants...)
		seenCommits := make(map[string]bool)
		boundarySet := newOrdSet()
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			obj := lookup(oid)
			if obj == nil {
				continue
			}
			if obj.Type == "tag" {
				include.add(oid)
				if target := ParseTagTarget(obj.Data); target != "" {
					stack = append(stack, target)
				}
				continue
			}
			if obj.Type != "commit" || seenCommits[oid] {
				continue
			}
			seenCommits[oid] = true
			commit := ParseCommit(obj.Data)
			if commit.Committer.Time > int64(*opts.deepenSince) {
				include.add(oid)
				stack = append(stack, commit.Parents...)
			} else {
				boundarySet.add(oid)
			}
		}
		// Add the tree closure of every included commit.
		for _, oid := range append([]string(nil), include.order...) {
			if obj := lookup(oid); obj != nil && obj.Type == "commit" {
				for _, reached := range collectReachableOrdered(lookup, []string{ParseCommit(obj.Data).Tree}) {
					include.add(reached)
				}
			}
		}
		includeOrder = include.order
		includeSet = include.seen
		boundary = boundarySet.order
	default:
		includeOrder = collectReachableOrdered(lookup, opts.wants)
		includeSet = make(map[string]bool, len(includeOrder))
		for _, oid := range includeOrder {
			includeSet[oid] = true
		}
	}

	toSend := []PackObject{}
	for _, oid := range includeOrder {
		if haveSet[oid] {
			continue
		}
		if obj := lookup(oid); obj != nil {
			toSend = append(toSend, PackObject{Oid: oid, Type: obj.Type, Data: obj.Data})
		}
	}
	// Commits first, then trees, then blobs: a stable, ancestry-friendly order.
	rank := map[string]int{"commit": 0, "tag": 1, "tree": 2, "blob": 3}
	sort.SliceStable(toSend, func(i, j int) bool { return rank[toSend[i].Type] < rank[toSend[j].Type] })

	pack := WritePack(toSend)
	boundarySet := make(map[string]bool, len(boundary))
	for _, oid := range boundary {
		boundarySet[oid] = true
	}
	unshallowLines := []string{}
	for _, oid := range opts.clientShallow {
		if includeSet[oid] && !boundarySet[oid] {
			unshallowLines = append(unshallowLines, oid)
		}
	}
	plan := &fetchPlan{
		pack:           pack,
		framed:         bytes.Join(sidebandPackets(pack), nil),
		shallowLines:   boundary,
		unshallowLines: unshallowLines,
		commonHaves:    commonHaves,
		hasHaves:       len(opts.haves) > 0,
	}
	planCachesMu.Lock()
	if len(perRepo) >= planCacheMax {
		for k := range perRepo {
			delete(perRepo, k)
		}
	}
	perRepo[cacheKey] = plan
	planCachesMu.Unlock()
	return plan
}

func sidebandPackets(pack []byte) [][]byte {
	packets := [][]byte{}
	const chunk = 65515
	for offset := 0; offset < len(pack); offset += chunk {
		end := offset + chunk
		if end > len(pack) {
			end = len(pack)
		}
		payload := make([]byte, 0, end-offset+1)
		payload = append(payload, 1)
		payload = append(payload, pack[offset:end]...)
		packets = append(packets, PktLineBytes(payload))
	}
	if len(packets) == 0 {
		packets = append(packets, PktLineBytes([]byte{1}))
	}
	return packets
}

// jsParseInt mirrors Number.parseInt(s, 10): an optional sign and a leading run of digits.
// ok is false when there are no digits (JS NaN).
func jsParseInt(s string) (value int, ok bool) {
	s = strings.TrimSpace(s)
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	v := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		v = v*10 + int(s[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}

func fetchV2(repo *RepoRec, args []string, send Sender) {
	var wants, haves, clientShallow []string
	var depth, deepenSince *int
	noProgress := false
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			wants = append(wants, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "shallow "):
			clientShallow = append(clientShallow, strings.TrimSpace(arg[8:]))
		case strings.HasPrefix(arg, "deepen "):
			if v, ok := jsParseInt(arg[7:]); ok {
				depth = &v
			} else {
				// JS NaN depth: d >= NaN is always false, so CollectShallow walks
				// everything and finds no boundary — a huge depth is equivalent.
				v := 1 << 30
				depth = &v
			}
		case strings.HasPrefix(arg, "deepen-since "):
			if v, ok := jsParseInt(arg[13:]); ok {
				deepenSince = &v
			} else {
				// JS NaN cutoff: committerTime > NaN is always false, so every
				// commit lands on the boundary — a huge cutoff is equivalent.
				v := int(^uint(0) >> 1)
				deepenSince = &v
			}
		case arg == "no-progress":
			noProgress = true
		}
	}
	_ = noProgress // the client asked for no-progress, so band 1 carries everything
	plan := planFetch(repo, fetchOpts{
		wants: wants, haves: haves, clientShallow: clientShallow,
		depth: depth, deepenSince: deepenSince,
	})
	var buf bytes.Buffer
	// acknowledgments section (section headers carry their trailing newline)
	buf.Write(PktLine("acknowledgments\n"))
	if !plan.hasHaves {
		buf.Write(PktLine("NAK\n"))
	} else {
		for _, oid := range plan.commonHaves {
			buf.Write(PktLine("ACK " + oid + " common\n"))
		}
		if len(plan.commonHaves) > 0 {
			buf.Write(PktLine("ready\n"))
		} else {
			buf.Write(PktLine("NAK\n"))
		}
	}
	buf.Write(FlushPkt)
	// shallow-info section, only when shallowness is in play
	if depth != nil || deepenSince != nil || len(clientShallow) > 0 {
		buf.Write(PktLine("shallow-info\n"))
		for _, oid := range plan.shallowLines {
			buf.Write(PktLine("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallowLines {
			buf.Write(PktLine("unshallow " + oid + "\n"))
		}
		buf.Write(FlushPkt)
	}
	// packfile section
	buf.Write(PktLine("packfile\n"))
	buf.Write(plan.framed)
	buf.Write(FlushPkt)
	send(200, "application/x-git-upload-pack-result", buf.Bytes())
}

func fetchV0(repo *RepoRec, firstSegment, secondSegment []string, send Sender) {
	var wants, haves, clientShallow []string
	var depth *int
	for _, line := range firstSegment {
		parts := strings.Split(line, " ")
		if len(parts) < 2 {
			continue
		}
		switch parts[0] {
		case "want":
			wants = append(wants, parts[1])
		case "shallow":
			clientShallow = append(clientShallow, parts[1])
		case "deepen":
			if v, ok := jsParseInt(parts[1]); ok {
				depth = &v
			} else {
				v := 1 << 30
				depth = &v
			}
		}
	}
	for _, line := range secondSegment {
		parts := strings.Split(line, " ")
		if len(parts) >= 2 && parts[0] == "have" {
			haves = append(haves, parts[1])
		}
	}
	plan := planFetch(repo, fetchOpts{
		wants: wants, haves: haves, clientShallow: clientShallow, depth: depth,
	})
	var buf bytes.Buffer
	for _, oid := range plan.shallowLines {
		buf.Write(PktLine("shallow " + oid))
	}
	for _, oid := range plan.unshallowLines {
		buf.Write(PktLine("unshallow " + oid))
	}
	if len(plan.commonHaves) > 0 {
		for _, oid := range plan.commonHaves {
			buf.Write(PktLine("ACK " + oid + " common"))
		}
		buf.Write(PktLine("ACK " + plan.commonHaves[len(plan.commonHaves)-1]))
	} else {
		buf.Write(PktLine("NAK"))
	}
	buf.Write(plan.framed)
	buf.Write(FlushPkt)
	send(200, "application/x-git-upload-pack-result", buf.Bytes())
}

// stripOneTrailingNewline mirrors the TS .replace(/\n$/, ”).
func stripOneTrailingNewline(s string) string {
	return strings.TrimSuffix(s, "\n")
}

func segmentText(lines [][]byte) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = stripOneTrailingNewline(string(line))
	}
	return out
}

func findDelim(lines [][]byte) int {
	for i, line := range lines {
		if len(line) == 1 && line[0] == 1 {
			return i
		}
	}
	return -1
}

// UploadPack serves git-upload-pack POST bodies: v2 ls-refs / fetch commands and v0 fetch.
func UploadPack(repo *RepoRec, body []byte, send Sender) {
	if repo == nil {
		sendText(send, 404, "text/plain", "repository not found\n")
		return
	}
	defer func() {
		if recover() != nil {
			sendText(send, 400, "text/plain", "invalid upload-pack request\n")
		}
	}()
	first, err := ReadPktSegment(body, 0)
	if err != nil {
		sendText(send, 400, "text/plain", "invalid upload-pack request\n")
		return
	}
	firstText := segmentText(first.Lines)
	command := ""
	if len(firstText) > 0 {
		command = firstText[0]
	}
	switch {
	case command == "command=ls-refs":
		args := []string{}
		if delim := findDelim(first.Lines); delim >= 0 {
			args = firstText[delim+1:]
		}
		lsRefs(repo, args, send)
	case command == "command=fetch":
		var args []string
		if delim := findDelim(first.Lines); delim >= 0 {
			args = firstText[delim+1:]
		} else {
			args = firstText[1:]
		}
		fetchV2(repo, args, send)
	case strings.HasPrefix(command, "want ") || command == "":
		// Protocol v0 fetch: wants segment, then a haves segment.
		secondText := []string{}
		if second, serr := ReadPktSegment(body, first.Rest); serr == nil {
			secondText = segmentText(second.Lines)
		}
		fetchV0(repo, firstText, secondText, send)
	default:
		sendText(send, 400, "text/plain", "unsupported upload-pack request\n")
	}
}

// ---------------------------------------------------------------------------
// receive-pack

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

// ReceivePack serves git-receive-pack POST bodies: ref updates plus a pack, with per-ref
// report-status lines and fast-forward-only enforcement for existing refs.
func ReceivePack(repo *RepoRec, body []byte, send Sender) {
	if repo == nil {
		sendText(send, 404, "text/plain", "repository not found\n")
		return
	}
	commandSegment, err := ReadPktSegment(body, 0)
	if err != nil {
		sendText(send, 400, "text/plain", "invalid receive-pack request\n")
		return
	}
	rest := commandSegment.Rest
	commandLines := segmentText(commandSegment.Lines)
	commands := []pushCommand{}
	clientCaps := ""
	for i, rawLine := range commandLines {
		line := rawLine
		if i == 0 {
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				clientCaps = line[nul+1:]
				line = line[:nul]
			}
		}
		parts := strings.Split(line, " ")
		if len(parts) == 3 && IsSha40(parts[0]) && IsSha40(parts[1]) {
			commands = append(commands, pushCommand{oldOid: parts[0], newOid: parts[1], ref: parts[2]})
		}
	}

	lookup := RepoLookup(repo)
	needsPack := false
	for _, command := range commands {
		if command.newOid != ZeroOID {
			needsPack = true
			break
		}
	}
	unpackOk := true
	unpackError := ""
	fresh := make(map[string]*GitObject)
	if needsPack {
		packBytes := body[rest:]
		if objects, perr := ReadPack(packBytes, lookup); perr != nil {
			unpackOk = false
			unpackError = perr.Error()
		} else {
			for _, obj := range objects {
				o := obj
				fresh[o.Oid] = &GitObject{Type: o.Type, Data: o.Data}
			}
		}
	}
	unionLookup := func(oid string) *GitObject {
		if obj, ok := fresh[oid]; ok {
			return obj
		}
		return lookup(oid)
	}

	type pushResult struct {
		ref    string
		ok     bool
		reason string
	}
	results := []pushResult{}
	if !unpackOk {
		for _, command := range commands {
			results = append(results, pushResult{ref: command.ref, ok: false, reason: "unpack failed"})
		}
	} else {
		// Commit the fresh objects first so ancestry checks and later reads see them.
		for oid, obj := range fresh {
			if _, exists := repo.Objects[oid]; !exists {
				repo.Objects[oid] = obj
			}
		}
		for _, command := range commands {
			ref, oldOid, newOid := command.ref, command.oldOid, command.newOid
			if !strings.HasPrefix(ref, "refs/") {
				results = append(results, pushResult{ref, false, "invalid ref"})
				continue
			}
			current, ok := repo.Refs[ref]
			if !ok {
				current = ZeroOID
			}
			if current != oldOid {
				results = append(results, pushResult{ref, false, "stale info"})
				continue
			}
			if newOid == ZeroOID {
				delete(repo.Refs, ref)
				repo.RefsVersion++
				results = append(results, pushResult{ref, true, ""})
				continue
			}
			target := unionLookup(newOid)
			if target == nil {
				results = append(results, pushResult{ref, false, "missing object"})
				continue
			}
			if strings.HasPrefix(ref, "refs/heads/") && target.Type != "commit" {
				results = append(results, pushResult{ref, false, "invalid object type"})
				continue
			}
			if strings.HasPrefix(ref, "refs/tags/") && target.Type != "commit" && target.Type != "tag" {
				results = append(results, pushResult{ref, false, "invalid object type"})
				continue
			}
			if oldOid != ZeroOID {
				// Only a commit target gets the fast-forward check; a tag target
				// skips it entirely (faithful to the TS).
				if target.Type == "commit" && !IsAncestor(unionLookup, oldOid, newOid) {
					results = append(results, pushResult{ref, false, "non-fast-forward"})
					continue
				}
			}
			repo.Refs[ref] = newOid
			repo.RefsVersion++
			results = append(results, pushResult{ref, true, ""})
		}
	}

	reportLines := [][]byte{}
	if unpackOk {
		reportLines = append(reportLines, PktLine("unpack ok\n"))
	} else {
		errText := unpackError
		if errText == "" {
			errText = "error"
		}
		reportLines = append(reportLines, PktLine("unpack "+errText+"\n"))
	}
	for _, result := range results {
		if result.ok {
			reportLines = append(reportLines, PktLine("ok "+result.ref+"\n"))
		} else {
			reportLines = append(reportLines, PktLine("ng "+result.ref+" "+result.reason+"\n"))
		}
	}
	wantsSideband := false
	for _, cap := range strings.Split(clientCaps, " ") {
		if cap == "side-band-64k" {
			wantsSideband = true
			break
		}
	}
	var payload bytes.Buffer
	if wantsSideband {
		inner := bytes.Join(reportLines, nil)
		const chunk = 65515
		for offset := 0; offset < len(inner); offset += chunk {
			end := offset + chunk
			if end > len(inner) {
				end = len(inner)
			}
			band := make([]byte, 0, end-offset+1)
			band = append(band, 1)
			band = append(band, inner[offset:end]...)
			payload.Write(PktLineBytes(band))
		}
		payload.Write(FlushPkt)
	} else {
		for _, line := range reportLines {
			payload.Write(line)
		}
		payload.Write(FlushPkt)
	}
	send(200, "application/x-git-receive-pack-result", payload.Bytes())
}
