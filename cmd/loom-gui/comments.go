package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/henricktissink/loom/internal/gitdiff"
)

// Review comments: the operator comments on diff lines, sends the open ones
// to the session as one line-anchored message, and keeps a state per
// comment (open, sent, resolved, invalid with a reason). The same notes
// file holds the files marked viewed (with the patch hash they were viewed
// at, so a changed file re-flags) and the states of second-opinion
// findings. One file per Claude session id, in ~/.loom/comments/<sid>.json,
// so the notes follow the session across a resume.

// Comment states.
const (
	cmOpen     = "open"
	cmSent     = "sent"
	cmResolved = "resolved"
	cmInvalid  = "invalid"
)

// Comment is one note on a diff line or range.
type Comment struct {
	ID string `json:"id"`
	// Repo is the directory whose diff it was made in; File is relative to it.
	Repo string `json:"repo"`
	File string `json:"file"`
	// Side: "new" lines (added or context) number in the new file, "old"
	// (removed) lines in the old one. EndLine is the range's last line.
	Side    string `json:"side"`
	Line    int    `json:"line"`
	EndLine int    `json:"endLine"`
	// Quote is the code commented on, as the diff showed it (no +/- marks):
	// it anchors the comment when the line numbers move, and goes into the
	// message so the session sees what was meant.
	Quote   string `json:"quote"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"` // why it is invalid
	Created int64  `json:"created"`
	Sent    int64  `json:"sent,omitempty"`
}

// FindingMark is the operator's verdict on a second-opinion finding.
type FindingMark struct {
	State  string `json:"state"` // fixed | invalid | commented (made a review comment)
	Reason string `json:"reason,omitempty"`
}

// NotesDTO is a session's review notes.
type NotesDTO struct {
	Comments []Comment `json:"comments"`
	// Viewed maps viewedKey(scope, repo, file) to the patch hash it was
	// viewed at. Per diff scope: the two scopes' patches of a file differ.
	Viewed   map[string]string      `json:"viewed"`
	Findings map[string]FindingMark `json:"findings"`
}

func viewedKey(scope, repo, file string) string { return scope + "\x00" + repo + "\x00" + file }

// FindingKey names a second-opinion finding across reruns: the same file,
// line and summary is the same finding.
func FindingKey(file string, line int, summary string) string {
	return fmt.Sprintf("%s:%d:%s", file, line, summary)
}

type commentStore struct {
	mu  sync.Mutex
	dir string // "" keeps notes in memory only
	mem map[string]*NotesDTO
	now func() time.Time
}

func newCommentStore(dir string, now func() time.Time) *commentStore {
	return &commentStore{dir: dir, mem: map[string]*NotesDTO{}, now: now}
}

// load is sid's notes; caller holds mu. Never nil.
func (s *commentStore) load(sid string) *NotesDTO {
	if n := s.mem[sid]; n != nil {
		return n
	}
	n := &NotesDTO{}
	if s.dir != "" {
		if b, err := os.ReadFile(filepath.Join(s.dir, sid+".json")); err == nil {
			_ = json.Unmarshal(b, n)
		}
	}
	if n.Comments == nil {
		n.Comments = []Comment{}
	}
	if n.Viewed == nil {
		n.Viewed = map[string]string{}
	}
	if n.Findings == nil {
		n.Findings = map[string]FindingMark{}
	}
	s.mem[sid] = n
	return n
}

// save writes sid's notes; caller holds mu.
func (s *commentStore) save(sid string, n *NotesDTO) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(s.dir, sid+".json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// update applies f to sid's notes and saves them; a failed save leaves the
// notes as they were.
func (s *commentStore) update(sid string, f func(n *NotesDTO) error) (NotesDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.load(sid)
	next := clone(n)
	if err := f(&next); err != nil {
		return clone(n), err
	}
	if err := s.save(sid, &next); err != nil {
		return clone(n), err
	}
	*n = next
	return clone(n), nil
}

func (s *commentStore) get(sid string) NotesDTO {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.load(sid))
}

func clone(n *NotesDTO) NotesDTO {
	c := NotesDTO{Comments: append([]Comment{}, n.Comments...), Viewed: map[string]string{}, Findings: map[string]FindingMark{}}
	for k, v := range n.Viewed {
		c.Viewed[k] = v
	}
	for k, v := range n.Findings {
		c.Findings[k] = v
	}
	return c
}

func newCommentID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *commentStore) add(sid string, c Comment) (NotesDTO, error) {
	c.Body = strings.TrimSpace(c.Body)
	if c.Body == "" {
		return NotesDTO{}, errors.New("the comment is empty")
	}
	if c.File == "" || c.Line <= 0 {
		return NotesDTO{}, errors.New("a comment needs a file and a line")
	}
	if c.Side != "old" {
		c.Side = "new"
	}
	if c.EndLine < c.Line {
		c.EndLine = c.Line
	}
	c.ID, c.State, c.Reason, c.Sent = newCommentID(), cmOpen, "", 0
	c.Created = s.now().Unix()
	return s.update(sid, func(n *NotesDTO) error {
		n.Comments = append(n.Comments, c)
		return nil
	})
}

// edit changes an open comment's text.
func (s *commentStore) edit(sid, id, body string) (NotesDTO, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return NotesDTO{}, errors.New("the comment is empty")
	}
	return s.update(sid, func(n *NotesDTO) error {
		c := find(n, id)
		if c == nil {
			return errors.New("no such comment")
		}
		c.Body = body
		return nil
	})
}

// setState moves a comment to state. Invalid needs a reason: it is the
// record of why the comment was wrong. Open clears the sent time, so the
// comment goes out again with the next send.
func (s *commentStore) setState(sid, id, state, reason string) (NotesDTO, error) {
	reason = strings.TrimSpace(reason)
	switch state {
	case cmOpen, cmResolved:
	case cmInvalid:
		if reason == "" {
			return NotesDTO{}, errors.New("say why the comment is invalid")
		}
	default:
		return NotesDTO{}, fmt.Errorf("unknown comment state %q", state)
	}
	return s.update(sid, func(n *NotesDTO) error {
		c := find(n, id)
		if c == nil {
			return errors.New("no such comment")
		}
		c.State, c.Reason = state, ""
		if state == cmInvalid {
			c.Reason = reason
		}
		if state == cmOpen {
			c.Sent = 0
		}
		return nil
	})
}

func (s *commentStore) remove(sid, id string) (NotesDTO, error) {
	return s.update(sid, func(n *NotesDTO) error {
		for i := range n.Comments {
			if n.Comments[i].ID == id {
				n.Comments = append(n.Comments[:i], n.Comments[i+1:]...)
				return nil
			}
		}
		return errors.New("no such comment")
	})
}

// markSent moves the given open comments to sent.
func (s *commentStore) markSent(sid string, ids []string) (NotesDTO, error) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	at := s.now().Unix()
	return s.update(sid, func(n *NotesDTO) error {
		for i := range n.Comments {
			if c := &n.Comments[i]; want[c.ID] && c.State == cmOpen {
				c.State, c.Sent = cmSent, at
			}
		}
		return nil
	})
}

// setViewed marks a file viewed in a diff scope at its patch hash; an
// empty hash unmarks it.
func (s *commentStore) setViewed(sid, scope, repo, file, hash string) (NotesDTO, error) {
	if scope != "since" && scope != "uncommitted" {
		return NotesDTO{}, fmt.Errorf("unknown diff scope %q", scope)
	}
	k := viewedKey(scope, repo, file)
	return s.update(sid, func(n *NotesDTO) error {
		if hash == "" {
			delete(n.Viewed, k)
		} else {
			n.Viewed[k] = hash
		}
		return nil
	})
}

// setFinding records a verdict on a second-opinion finding: fixed, invalid
// (with a reason), commented (it became a review comment), or "" to clear it.
func (s *commentStore) setFinding(sid, key, state, reason string) (NotesDTO, error) {
	reason = strings.TrimSpace(reason)
	switch state {
	case "", "fixed", "commented":
	case cmInvalid:
		if reason == "" {
			return NotesDTO{}, errors.New("say why the finding is invalid")
		}
	default:
		return NotesDTO{}, fmt.Errorf("unknown finding state %q", state)
	}
	return s.update(sid, func(n *NotesDTO) error {
		if state == "" {
			delete(n.Findings, key)
		} else {
			n.Findings[key] = FindingMark{State: state, Reason: reason}
		}
		return nil
	})
}

func find(n *NotesDTO, id string) *Comment {
	for i := range n.Comments {
		if n.Comments[i].ID == id {
			return &n.Comments[i]
		}
	}
	return nil
}

// commentsPrompt composes the open comments as one message: each with its
// place, the code as quoted, and the note. Paths are relative to the
// session's directory; a comment in another of its repos keeps that repo's
// path in front. Ordered by file and line so the session can work down it.
func commentsPrompt(cwd string, cs []Comment) (string, []string) {
	open := []Comment{}
	for _, c := range cs {
		if c.State == cmOpen {
			open = append(open, c)
		}
	}
	if len(open) == 0 {
		return "", nil
	}
	sort.SliceStable(open, func(i, j int) bool {
		a, b := open[i], open[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	var b strings.Builder
	n := len(open)
	fmt.Fprintf(&b, "Review comments on your changes (%d). Work through each one. If you disagree with one, say why instead of changing the code. When you are done, list each comment with what you did.\n", n)
	ids := make([]string, 0, n)
	for i, c := range open {
		ids = append(ids, c.ID)
		path := c.File
		if c.Repo != "" && c.Repo != cwd {
			path = filepath.Join(c.Repo, c.File)
		}
		where := fmt.Sprintf("%s:%d", path, c.Line)
		if c.EndLine > c.Line {
			where = fmt.Sprintf("%s:%d-%d", path, c.Line, c.EndLine)
		}
		if c.Side == "old" {
			where += " (removed lines; numbers are in the old file)"
		}
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, where)
		if q := strings.TrimRight(c.Quote, "\n"); q != "" {
			fence := "```"
			for strings.Contains(q, fence) {
				fence += "`"
			}
			fmt.Fprintf(&b, "%s\n%s\n%s\n", fence, q, fence)
		}
		b.WriteString(c.Body + "\n")
	}
	return b.String(), ids
}

// ---- App methods ----

func (a *App) notesTarget(name string) (sid, cwd string, err error) {
	if a.notes == nil {
		return "", "", errors.New("review comments are not available")
	}
	return a.sessionReviewTarget(name)
}

// ReviewNotes is a session's comments, viewed files and finding verdicts.
func (a *App) ReviewNotes(name string) (NotesDTO, error) {
	sid, _, err := a.notesTarget(name)
	if err != nil {
		return NotesDTO{}, err
	}
	return a.notes.get(sid), nil
}

// AddComment adds an open comment on a diff line or range.
func (a *App) AddComment(name string, c Comment) (NotesDTO, error) {
	sid, _, err := a.notesTarget(name)
	if err != nil {
		return NotesDTO{}, err
	}
	return a.notes.add(sid, c)
}

// EditComment changes a comment's text.
func (a *App) EditComment(name, id, body string) (NotesDTO, error) {
	sid, _, err := a.notesTarget(name)
	if err != nil {
		return NotesDTO{}, err
	}
	return a.notes.edit(sid, id, body)
}

// SetCommentState marks a comment open, resolved, or invalid with a reason.
func (a *App) SetCommentState(name, id, state, reason string) (NotesDTO, error) {
	sid, _, err := a.notesTarget(name)
	if err != nil {
		return NotesDTO{}, err
	}
	return a.notes.setState(sid, id, state, reason)
}

// DeleteComment removes a comment.
func (a *App) DeleteComment(name, id string) (NotesDTO, error) {
	sid, _, err := a.notesTarget(name)
	if err != nil {
		return NotesDTO{}, err
	}
	return a.notes.remove(sid, id)
}

// SetViewed marks a file viewed in a diff scope ("since" or
// "uncommitted") at its patch hash ("" unmarks it).
func (a *App) SetViewed(name, scope, repo, file, hash string) (NotesDTO, error) {
	sid, _, err := a.notesTarget(name)
	if err != nil {
		return NotesDTO{}, err
	}
	return a.notes.setViewed(sid, scope, repo, file, hash)
}

// SetFindingState records fixed or invalid (with a reason) on a
// second-opinion finding, or clears it with "".
func (a *App) SetFindingState(name, key, state, reason string) (NotesDTO, error) {
	sid, _, err := a.notesTarget(name)
	if err != nil {
		return NotesDTO{}, err
	}
	return a.notes.setFinding(sid, key, state, reason)
}

// CommentsPrompt is the message the open comments would go out as.
func (a *App) CommentsPrompt(name string) (string, error) {
	sid, cwd, err := a.notesTarget(name)
	if err != nil {
		return "", err
	}
	p, _ := commentsPrompt(cwd, a.notes.get(sid).Comments)
	if p == "" {
		return "", errors.New("no open comments to send")
	}
	return p, nil
}

// SendComments sends text (the composed comments, as the operator edited
// it) to the live session as one message and marks the comments that were
// open when it was composed as sent.
func (a *App) SendComments(name, text string) error {
	sid, cwd, err := a.notesTarget(name)
	if err != nil {
		return err
	}
	_, ids := commentsPrompt(cwd, a.notes.get(sid).Comments)
	if err := a.SendPrompt(name, text); err != nil {
		return err
	}
	_, err = a.notes.markSent(sid, ids)
	return err
}

// SessionDiffSince is a session's changes since its task started, per
// directory: the commits made on top of the commit HEAD was at then (on a
// worktree branch, since the branch was cut), plus uncommitted changes and
// new files. Without a task (or a findable base) it is the uncommitted
// changes, and Base says which.
func (a *App) SessionDiffSince(name string) (DiffDTO, error) {
	e, ok := a.railRow(name)
	if !ok {
		return DiffDTO{}, fmt.Errorf("%s is not on the rail any more", name)
	}
	row := e.row
	var started int64
	if row.HasTask {
		started = row.Task.StartedAt
	}
	branch := ""
	if e.dto.Worktree != nil {
		branch = e.dto.Worktree.Branch
	}
	labels := a.targetLabels()
	out := DiffDTO{Repos: []RepoDiff{}}
	var ms []MustReadDTO
	for _, dir := range sessionDirs(row.SessionRow) {
		br := ""
		if dir == row.Cwd {
			br = branch
		}
		base, scope := diffBase(dir, br, started)
		// Against at HEAD too, so new files' contents are in the patch (the
		// review sheet's must-read files are marked viewed there); a repo
		// with no commit yet has no base, and gets the working tree
		d := gitdiff.Against(dir, base)
		if d.Error != "" && base == "HEAD" {
			d = gitDiff(dir)
		} else {
			d.Dirty = d.Error == "" && (d.Patch != "" || len(d.Untracked) > 0)
		}
		d.Label = labels[dir]
		if d.Label == "" {
			d.Label = filepath.Base(dir)
		}
		out.Repos = append(out.Repos, d)
		out.Scopes = append(out.Scopes, scope)
		ms = append(ms, mustReadIn(dir, d.Label, d.Files, d.Untracked)...)
	}
	out.MustRead = mustReadMap(ms)
	return out, nil
}
