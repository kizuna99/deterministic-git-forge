package main

// The forge's in-memory store: one viewer, their repositories, and inside each repository a
// private git object store plus the issue / pull / label records the REST, GraphQL and MCP
// surfaces share. Every timestamp comes from a logical clock seeded by MAKEFUN_TRIAL_NOW, so a
// replayed run of the same request sequence produces the same bytes.
//
// Port of entry/store.ts.

import (
	"encoding/base64"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Viewer identity (store.ts VIEWER_* constants).
const ViewerLogin = "adaiversen"
const ViewerID = 1000
const ViewerName = "ada iversen"
const ViewerEmail = "adaiversen@example.invalid"

// StoreMu is the global store lock; handlers lock it per request.
var StoreMu sync.RWMutex

// LabelRec mirrors store.ts LabelRec.
type LabelRec struct {
	Name, Color, Description string
}

// CommentRec mirrors store.ts CommentRec.
type CommentRec struct {
	ID          int
	Body        string
	AuthorLogin string
	CreatedSeq  int
	CreatedAt   string
}

// EntryRec unifies store.ts IssueRec and PullRec. Fields that only apply to one kind stay at
// their zero value for the other (pulls carry HeadRef/BaseRef and the merge fields; issues
// leave them empty).
type EntryRec struct {
	Kind            string // "issue" | "pull"
	ID              int
	Number          int
	Title           string
	Body            string
	State           string // "open" | "closed"
	Labels          []string
	HeadRef         string
	BaseRef         string
	HeadShaSnapshot string
	BaseShaSnapshot string
	Merged          bool
	MergedAt        string
	HasMergedAt     bool
	MergeCommitSha  string
	HasMergeSha     bool
	Comments        []CommentRec
	CreatedSeq      int
	CreatedAt       string
	UpdatedAt       string
}

// RepoRec mirrors store.ts RepoRec.
type RepoRec struct {
	ID             int
	Owner          string
	Name           string
	Description    string
	IsPrivate      bool
	DefaultBranch  string
	CreatedSeq     int
	CreatedAt      string
	UpdatedAt      string
	Objects        map[string]*GitObject
	Refs           map[string]string
	Labels         map[string]*LabelRec
	LabelsVersion  int
	RefsVersion    int
	EntriesVersion int
	Entries        map[int]*EntryRec
	NextNumber     int
}

// Store mirrors the TS Store class.
type Store struct {
	Repos     map[string]*RepoRec
	ReposByID map[int]*RepoRec

	nextRepoID   int
	nextEntityID int
	seq          int
	clockSec     int64

	isoCacheSec   int64
	isoCache      string
	isoCacheValid bool
}

// NewStore builds a store whose logical clock is seeded from MAKEFUN_TRIAL_NOW (falling back
// to 1735689600 when the variable is unset or unparseable, exactly like the TS constructor).
func NewStore() *Store {
	s := &Store{
		Repos:        make(map[string]*RepoRec),
		ReposByID:    make(map[int]*RepoRec),
		nextRepoID:   1,
		nextEntityID: 1,
		clockSec:     1735689600,
	}
	if sec, ok := parseTrialNow(os.Getenv("MAKEFUN_TRIAL_NOW")); ok {
		s.clockSec = sec
	}
	return s
}

// parseTrialNow mirrors JS Date.parse for the ISO-ish forms the trial harness uses: it returns
// whole seconds (Math.floor(parsed/1000)) and reports failure for anything unparseable —
// including bare numbers, which Date.parse rejects.
func parseTrialNow(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2006/01/02 15:04:05",
		"2006/01/02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}

// S is the store singleton.
var S = NewStore()

// NextSeq returns the next monotonic sequence for creation ordering across the whole store.
func (s *Store) NextSeq() int {
	s.seq++
	return s.seq
}

func (s *Store) isoFor(sec int64) string {
	if !s.isoCacheValid || sec != s.isoCacheSec {
		s.isoCacheSec = sec
		// JS Date#toISOString: always millisecond precision, always UTC ('...T...ss.000Z'
		// here because the clock only ever holds whole seconds).
		s.isoCache = time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000") + "Z"
		s.isoCacheValid = true
	}
	return s.isoCache
}

// Tick advances the logical clock by one second and returns the new time.
func (s *Store) Tick() (sec int64, iso string) {
	s.clockSec++
	return s.clockSec, s.isoFor(s.clockSec)
}

// NowISO returns the current logical time without advancing the clock.
func (s *Store) NowISO() string {
	return s.isoFor(s.clockSec)
}

// AllocEntityID returns the next global entity id.
func (s *Store) AllocEntityID() int {
	id := s.nextEntityID
	s.nextEntityID++
	return id
}

// CreateRepo creates and registers a repository, stamping it from the logical clock.
func (s *Store) CreateRepo(owner, name, desc string, priv bool) *RepoRec {
	_, nowISO := s.Tick()
	repo := &RepoRec{
		ID:            s.nextRepoID,
		Owner:         owner,
		Name:          name,
		Description:   desc,
		IsPrivate:     priv,
		DefaultBranch: "main",
		CreatedSeq:    s.NextSeq(),
		CreatedAt:     nowISO,
		UpdatedAt:     nowISO,
		Objects:       make(map[string]*GitObject),
		Refs:          make(map[string]string),
		Labels:        make(map[string]*LabelRec),
		Entries:       make(map[int]*EntryRec),
		NextNumber:    1,
	}
	s.nextRepoID++
	s.Repos[owner+"/"+name] = repo
	s.ReposByID[repo.ID] = repo
	return repo
}

// GetRepo returns the named repository, or nil if absent.
func (s *Store) GetRepo(owner, name string) *RepoRec {
	return s.Repos[owner+"/"+name]
}

// DeleteRepo removes a repository from both indexes.
func (s *Store) DeleteRepo(r *RepoRec) {
	delete(s.Repos, r.Owner+"/"+r.Name)
	delete(s.ReposByID, r.ID)
}

// ListRepos returns the owner's repositories in creation (createdSeq) order.
func (s *Store) ListRepos(owner string) []*RepoRec {
	out := make([]*RepoRec, 0)
	for _, repo := range s.Repos {
		if repo.Owner == owner {
			out = append(out, repo)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

// ---------------------------------------------------------------------------
// Global ids: opaque base64 of a typed numeric id, stable for the life of the process.

var globalIDCache sync.Map // map[string]string, key "kind:id"

// GlobalID returns base64 of "v1:<kind>:<id>", cached.
func GlobalID(kind string, id int) string {
	key := kind + ":" + strconv.Itoa(id)
	if hit, ok := globalIDCache.Load(key); ok {
		return hit.(string)
	}
	value := base64.StdEncoding.EncodeToString([]byte("v1:" + kind + ":" + strconv.Itoa(id)))
	globalIDCache.Store(key, value)
	return value
}

// ParseGlobalID decodes a global id produced by GlobalID. It mirrors the TS check: the
// base64-decoded text must match ^v1:([a-z]+):(\d+)$.
func ParseGlobalID(s string) (kind string, id int, ok bool) {
	decoded, err := decodeBase64Lenient(s)
	if err != nil {
		return "", 0, false
	}
	text := string(decoded)
	if !strings.HasPrefix(text, "v1:") {
		return "", 0, false
	}
	rest := text[len("v1:"):]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return "", 0, false
	}
	k, digits := rest[:colon], rest[colon+1:]
	if k == "" || digits == "" {
		return "", 0, false
	}
	for _, c := range k {
		if c < 'a' || c > 'z' {
			return "", 0, false
		}
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return "", 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return "", 0, false
	}
	return k, n, true
}

// decodeBase64Lenient accepts the standard alphabet with or without padding, like
// Buffer.from(s, 'base64') does for well-formed input.
func decodeBase64Lenient(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// ---------------------------------------------------------------------------
// Per-repo git helpers

// RepoLookup returns a Lookup over the repo's object store (nil when the oid is absent).
func RepoLookup(repo *RepoRec) Lookup {
	return func(oid string) *GitObject {
		return repo.Objects[oid]
	}
}

// RepoPut returns a Put that hashes and stores objects in the repo (first write wins).
func RepoPut(repo *RepoRec) Put {
	return func(typ string, data []byte) string {
		oid := HashObject(typ, data)
		if _, exists := repo.Objects[oid]; !exists {
			repo.Objects[oid] = &GitObject{Type: typ, Data: data}
		}
		return oid
	}
}

// BranchTip returns the commit a branch ref currently names, or "" when unborn/missing.
// Faithful to the TS: despite its comment about peeling tags, branchTip only accepts a ref
// whose target object is itself a commit.
func BranchTip(repo *RepoRec, branch string) string {
	oid, ok := repo.Refs["refs/heads/"+branch]
	if !ok || oid == "" {
		return ""
	}
	obj := repo.Objects[oid]
	if obj == nil {
		return ""
	}
	if obj.Type == "commit" {
		return oid
	}
	return ""
}
