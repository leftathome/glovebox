package records

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

const (
	spoolDirName    = "spool"
	rejectedDirName = "rejected"
	nameTimeLayout  = "20060102T150405.000Z"
	tmpMarker       = ".tmp-"
)

// Entry is one pending or rejected spool file. Its fields are for the
// spool's own bookkeeping: ScopeKey and DataSubject must never reach a log
// line or a metric label; ScopePrefix and RecordType may.
type Entry struct {
	Name        string
	Path        string
	ObservedAt  time.Time // from the name; for an unreadable file, its mtime
	ID          string
	RecordType  string
	ScopeKey    string // the scope's parts as JSON: a grouping key, not the canonical form
	ScopePrefix string // "source=<v>/surface=<v>/": what a log line may say about the scope
	DataSubject string
	Size        int64
	Rejected    bool
	Unreadable  bool // a quarantined file whose content could not be read as a snapshot
}

// Spool is the directory of snapshots waiting to be sent. Safe for one
// writer (the poll) and one sender at a time; mu serialises them.
//
// Durability: a snapshot is written to a temp file, fsynced, closed,
// renamed into place, and the directory is fsynced before Put returns. A
// move to rejected/ fsyncs rejected/ before spool/, so a crash between the
// two leaves the file in both, and recovery keeps the rejected copy; a
// delete fsyncs the directory before it returns.
type Spool struct {
	fs            fsys
	dir, rejected string
	cfg           SpoolConfig
	m             *Metrics
	log           *slog.Logger
	now           func() time.Time

	mu         sync.Mutex
	cache      map[string]Entry     // by path; a name's content never changes
	unreadable map[string]time.Time // pending paths whose last read failed (not their content): name time, for max_age
	unknown    map[string]bool      // scope keys whose head was last answered 409 unknown_subject
	erased     map[string]bool      // subjects answered 410 erased_subject since start
	overMax    bool                 // over max_bytes with only newest-of-scope files left (logged once)
}

// OpenSpool creates spool/ and spool/rejected/ under stateDir (0700) and
// recovers from a crash: temp files are removed; a file in both spool/ and
// rejected/ (a crash during a move) is kept as rejected; files whose name
// or content is not a spool snapshot's are moved to rejected/ and counted.
// A file that cannot be read at all (an I/O error, not its content) is
// left in place and tried again later, as in every listing.
func OpenSpool(stateDir string, cfg SpoolConfig, m *Metrics, log *slog.Logger, now func() time.Time) (*Spool, error) {
	return openSpool(osFS{}, stateDir, cfg, m, log, now)
}

func openSpool(fsy fsys, stateDir string, cfg SpoolConfig, m *Metrics, log *slog.Logger, now func() time.Time) (*Spool, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return nil, errors.New("records: the spool needs an absolute state directory")
	}
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	s := &Spool{fs: fsy, dir: filepath.Join(stateDir, spoolDirName), cfg: cfg, m: m, log: log, now: now,
		cache: map[string]Entry{}, unreadable: map[string]time.Time{}, unknown: map[string]bool{}, erased: map[string]bool{}}
	s.rejected = filepath.Join(s.dir, rejectedDirName)
	if err := fsy.MkdirAll(s.rejected, 0o700); err != nil {
		return nil, fmt.Errorf("records: create spool: %w", err)
	}
	// The new directory entries themselves.
	for _, d := range []string{stateDir, s.dir} {
		if err := fsy.SyncDir(d); err != nil {
			return nil, fmt.Errorf("records: sync spool directory: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recoverLocked(); err != nil {
		return nil, err
	}
	s.refreshGaugesLocked()
	return s, nil
}

func fileName(snap journalapi.Snapshot) string {
	return snap.ObservedAt.UTC().Format(nameTimeLayout) + "-" + snap.SnapshotID + ".json"
}

// Put writes snap to the spool. Nothing is sent here. observed_at is
// normalised (UTC, milliseconds) so the file name and the content agree.
// A snapshot about a subject the journal has answered 410 erased_subject
// since start is not written (counted as dropped erased_subject).
func (s *Spool) Put(snap journalapi.Snapshot) error {
	snap.ObservedAt = snap.ObservedAt.UTC().Truncate(time.Millisecond)
	if _, err := uuid.Parse(snap.SnapshotID); err != nil || len(snap.Scope) == 0 || snap.ObservedAt.Year() < 1 || snap.ObservedAt.Year() > 9999 {
		s.m.writeError()
		return errors.New("records: a snapshot needs a UUID, a scope and an observed_at")
	}
	body, err := json.Marshal(snap)
	if err != nil {
		s.m.writeError()
		return fmt.Errorf("records: encode snapshot: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if subj := subjectOf(snap.Scope); subj != "" && s.erased[subj] {
		s.m.droppedN(DropErasedSubject, 1)
		return nil
	}
	path := filepath.Join(s.dir, fileName(snap))
	if err := s.writeAtomic(path, body); err != nil {
		s.m.writeError()
		return fmt.Errorf("records: write spool file: %w", err)
	}
	if err := s.enforceLocked(); err != nil {
		s.log.Warn("records spool bounds could not be applied", "error", errText(err))
	}
	s.refreshGaugesLocked()
	return nil
}

// writeAtomic: temp file, write, fsync, close, rename, fsync the
// directory. On a failure before the rename the temp file is removed; a
// crash leaves at most a temp file, which recovery removes.
func (s *Spool) writeAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s%s%d-%d", path, tmpMarker, os.Getpid(), time.Now().UnixNano())
	f, err := s.fs.CreateExcl(tmp, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = s.fs.Rename(tmp, path)
	}
	if err != nil {
		_ = s.fs.Remove(tmp)
		return err
	}
	return s.fs.SyncDir(filepath.Dir(path))
}

// header is what the spool reads back from a file.
type header struct {
	SnapshotID string                 `json:"snapshot_id"`
	RecordType string                 `json:"record_type"`
	Scope      []journalapi.ScopePart `json:"scope"`
	ObservedAt time.Time              `json:"observed_at"`
}

func parseName(name string) (time.Time, string, bool) {
	if !strings.HasSuffix(name, ".json") || len(name) != len(nameTimeLayout)+1+36+len(".json") || name[len(nameTimeLayout)] != '-' {
		return time.Time{}, "", false
	}
	ts, err := time.Parse(nameTimeLayout, name[:len(nameTimeLayout)])
	if err != nil {
		return time.Time{}, "", false
	}
	id := strings.TrimSuffix(name[len(nameTimeLayout)+1:], ".json")
	if u, err := uuid.Parse(id); err != nil || u.String() != id {
		return time.Time{}, "", false
	}
	return ts, id, true
}

// errMismatch marks a file whose name or content is not a spool
// snapshot's: it can only appear by hand or by disk corruption (Put writes
// only complete files), and no later read will change that.
var errMismatch = errors.New("records: not a spool snapshot")

// parseEntry reads one file. Its error is errMismatch for a name or
// content that is not a snapshot's, else the read's own error (which may
// pass: EMFILE, EIO).
func (s *Spool) parseEntry(dir string, fi fileInfo, rejected bool) (Entry, error) {
	path := filepath.Join(dir, fi.Name)
	if e, ok := s.cache[path]; ok {
		return e, nil
	}
	ts, id, ok := parseName(fi.Name)
	if !ok {
		return Entry{}, errMismatch
	}
	raw, err := s.fs.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	var h header
	if err := json.Unmarshal(raw, &h); err != nil || h.SnapshotID != id || !h.ObservedAt.Equal(ts) || len(h.Scope) == 0 {
		return Entry{}, errMismatch
	}
	key, _ := json.Marshal(h.Scope)
	e := Entry{Name: fi.Name, Path: path, ObservedAt: ts, ID: id, RecordType: h.RecordType, ScopeKey: string(key),
		ScopePrefix: scopePrefix(h.Scope), DataSubject: subjectOf(h.Scope), Size: int64(len(raw)), Rejected: rejected}
	s.cache[path] = e
	return e, nil
}

func subjectOf(scope []journalapi.ScopePart) string {
	for _, p := range scope {
		if p[0] == record.PartSubject {
			return p[1]
		}
	}
	return ""
}

// scopePrefix is the scope's source and surface parts in the journal's
// canonical form ("source=powerschool/surface=assignments/"): the only
// part of a scope a log line carries.
func scopePrefix(scope []journalapi.ScopePart) string {
	var b strings.Builder
	for _, p := range scope {
		if p[0] == record.PartSource || p[0] == "surface" {
			b.WriteString(p[0] + "=" + record.EscapeScopeValue(labelValue(p[1])) + "/")
		}
	}
	return b.String()
}

// labelValue keeps a source or surface value that is a plain identifier
// (the connector's own constant) and replaces anything else with "other".
func labelValue(v string) string {
	if v == "" || len(v) > 64 {
		return "other"
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return "other"
		}
	}
	return v
}

// Pending lists spool/ oldest first (observed_at, then name).
func (s *Spool) Pending() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listPendingLocked()
}

// Rejected lists spool/rejected/ oldest first.
func (s *Spool) Rejected() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listRejectedLocked()
}

// listPendingLocked reads spool/. A file whose name or content is not a
// snapshot's is quarantined in rejected/; one that cannot be read (an I/O
// error) is left out of this listing and kept, see pendingEntryLocked.
func (s *Spool) listPendingLocked() ([]Entry, error) {
	fis, err := s.fs.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Entry
	seen := map[string]bool{}
	for _, fi := range fis {
		if fi.Dir || strings.Contains(fi.Name, tmpMarker) {
			continue
		}
		seen[filepath.Join(s.dir, fi.Name)] = true
		if e, ok := s.pendingEntryLocked(fi); ok {
			out = append(out, e)
		}
	}
	for path := range s.unreadable {
		if !seen[path] {
			delete(s.unreadable, path)
		}
	}
	sortEntries(out)
	return out, nil
}

// pendingEntryLocked parses one file of spool/. A name or content that is
// not a snapshot's is quarantined. A read error (EMFILE, EIO, ...) says
// nothing about the file: it stays in spool/, is skipped for now and read
// again by the next listing; meanwhile max_age still ages it by its name
// (enforceLocked). The error's kind is logged once per file.
func (s *Spool) pendingEntryLocked(fi fileInfo) (Entry, bool) {
	e, err := s.parseEntry(s.dir, fi, false)
	path := filepath.Join(s.dir, fi.Name)
	switch {
	case err == nil:
		delete(s.unreadable, path)
		return e, true
	case errors.Is(err, errMismatch):
		s.quarantineLocked(fi.Name)
	case errors.Is(err, fs.ErrNotExist):
		// Removed since the directory was read.
	default:
		if _, logged := s.unreadable[path]; !logged {
			s.log.Warn("records spool file could not be read; keeping it to try again", "error", errText(err))
		}
		ts, _, _ := parseName(fi.Name) // the name parsed, or err would be errMismatch
		s.unreadable[path] = ts
	}
	return Entry{}, false
}

func (s *Spool) listRejectedLocked() ([]Entry, error) {
	fis, err := s.fs.ReadDir(s.rejected)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, fi := range fis {
		if fi.Dir || strings.Contains(fi.Name, tmpMarker) {
			continue
		}
		e, err := s.parseEntry(s.rejected, fi, true)
		if err != nil {
			// Quarantined as unreadable: aged by its name's time when the
			// name parses, else by its mtime; counted toward max_bytes.
			e = Entry{Name: fi.Name, Path: filepath.Join(s.rejected, fi.Name), ObservedAt: fi.ModTime.UTC(),
				Size: fi.Size, Rejected: true, Unreadable: true}
			if ts, _, ok := parseName(fi.Name); ok {
				e.ObservedAt = ts
			}
		}
		out = append(out, e)
	}
	sortEntries(out)
	return out, nil
}

func sortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool {
		if !es[i].ObservedAt.Equal(es[j].ObservedAt) {
			return es[i].ObservedAt.Before(es[j].ObservedAt)
		}
		return es[i].Name < es[j].Name
	})
}

// Delete removes a file that the journal has accepted or that must not be
// kept. The removal is durable when Delete returns nil.
func (s *Spool) Delete(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.removeLocked(e)
	s.refreshGaugesLocked()
	return err
}

func (s *Spool) removeLocked(e Entry) error {
	delete(s.cache, e.Path)
	delete(s.unreadable, e.Path)
	if err := s.fs.Remove(e.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return s.fs.SyncDir(filepath.Dir(e.Path))
}

// Reject moves a pending file to rejected/ and counts reason.
func (s *Spool) Reject(e Entry, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.moveToRejectedLocked(e.Name)
	if err == nil {
		s.m.rejectedOne(reason)
	}
	s.refreshGaugesLocked()
	return err
}

// moveToRejectedLocked renames spool/<name> into rejected/, then fsyncs
// rejected/ BEFORE spool/: a crash in between leaves the file in both
// directories (recovery keeps the rejected copy), never in neither.
func (s *Spool) moveToRejectedLocked(name string) error {
	from, to := filepath.Join(s.dir, name), filepath.Join(s.rejected, name)
	delete(s.cache, from)
	delete(s.unreadable, from)
	if err := s.fs.Rename(from, to); err != nil {
		return err
	}
	if err := s.fs.SyncDir(s.rejected); err != nil {
		return err
	}
	return s.fs.SyncDir(s.dir)
}

func (s *Spool) quarantineLocked(name string) {
	if err := s.moveToRejectedLocked(name); err != nil {
		s.log.Warn("records spool file could not be quarantined", "error", errText(err))
		return
	}
	s.m.rejectedOne(RejectUnreadable)
	s.log.Warn("records spool file could not be read as a snapshot; moved to rejected/", "reason", RejectUnreadable)
}

// Body returns the file's bytes, sent verbatim (A9).
func (s *Spool) Body(e Entry) ([]byte, error) { return s.fs.ReadFile(e.Path) }

func (s *Spool) recoverLocked() error {
	rfis, err := s.fs.ReadDir(s.rejected)
	if err != nil {
		return err
	}
	inRejected := map[string]bool{}
	for _, fi := range rfis {
		switch {
		case fi.Dir:
		case strings.Contains(fi.Name, tmpMarker):
			if err := s.fs.Remove(filepath.Join(s.rejected, fi.Name)); err != nil {
				return err
			}
		default:
			inRejected[fi.Name] = true
		}
	}
	pfis, err := s.fs.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, fi := range pfis {
		path := filepath.Join(s.dir, fi.Name)
		switch {
		case fi.Dir:
		case strings.Contains(fi.Name, tmpMarker), inRejected[fi.Name]:
			// A crash during a write, or after a move's rejected/ fsync:
			// the rejected copy is the decision that was made.
			if err := s.fs.Remove(path); err != nil {
				return err
			}
		default:
			// As in every listing: quarantined for its name or content,
			// never for a read that failed.
			s.pendingEntryLocked(fi)
		}
	}
	if err := s.fs.SyncDir(s.rejected); err != nil {
		return err
	}
	return s.fs.SyncDir(s.dir)
}

// refreshGaugesLocked recounts both directories.
func (s *Spool) refreshGaugesLocked() {
	if s.m == nil {
		return
	}
	pending, perr := s.listPendingLocked()
	rejected, rerr := s.listRejectedLocked()
	if perr != nil || rerr != nil {
		return
	}
	var bytes int64
	for _, e := range pending {
		bytes += e.Size
	}
	for _, e := range rejected {
		bytes += e.Size
	}
	oldest := 0.0
	if len(pending) > 0 {
		oldest = max(0, s.now().Sub(pending[0].ObservedAt).Seconds())
	}
	s.m.setGauges(len(pending), len(rejected), bytes, oldest)
}

// RefreshGauges recounts the spool for the gauges (the loop calls it every
// minute so oldest_seconds keeps growing while nothing happens).
func (s *Spool) RefreshGauges() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshGaugesLocked()
}

// errText keeps an error's text out of a log line when it could carry a
// path with a scope value: only the error's kind is logged.
func errText(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, fs.ErrExist):
		return "exists"
	}
	return "i/o error"
}
