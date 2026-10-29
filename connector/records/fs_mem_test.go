package records

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// memFS is an in-memory file system that models what survives a crash:
// a file's content survives only as far as it was fsynced, and a directory
// entry (create, rename, remove) only once its directory was fsynced. It
// can fail one operation (failOnce) or crash at an operation (crashAt):
// that operation and every later one fail and change nothing, as if the
// process had died there. Restart returns what a new process would find.
// Directories are durable at creation (the spool creates them once).
//
// Counted operations, the ones a crash can fall between: MkdirAll,
// CreateExcl, Write, Sync, Close, Rename, Remove, SyncDir. Reads are not
// counted.
type memFS struct {
	mu     sync.Mutex
	dirs   map[string]*memDir
	ops    int
	failAt int  // 1-based operation that fails; 0: none
	crash  bool // every operation from failAt on fails
	now    func() time.Time
	trace  []string
}

type memDir struct {
	live, durable map[string]*memFile
}

type memFile struct {
	data, durable []byte
	mtime         time.Time
}

var errInjected = errors.New("injected failure")

func newMemFS(now func() time.Time) *memFS {
	return &memFS{dirs: map[string]*memDir{}, now: now}
}

// step counts one operation and says whether it fails.
func (m *memFS) step(what string) error {
	m.ops++
	m.trace = append(m.trace, what)
	if m.failAt > 0 && (m.ops == m.failAt || (m.crash && m.ops > m.failAt)) {
		return errInjected
	}
	return nil
}

func (m *memFS) setFailOnce(n int) {
	m.mu.Lock()
	m.ops, m.failAt, m.crash, m.trace = 0, n, false, nil
	m.mu.Unlock()
}
func (m *memFS) setCrashAt(n int) {
	m.mu.Lock()
	m.ops, m.failAt, m.crash, m.trace = 0, n, true, nil
	m.mu.Unlock()
}
func (m *memFS) opCount() int { m.mu.Lock(); defer m.mu.Unlock(); return m.ops }

// Restart is the file system a new process finds after a crash: only what
// was made durable.
func (m *memFS) Restart() *memFS {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := newMemFS(m.now)
	copies := map[*memFile]*memFile{}
	for path, d := range m.dirs {
		nd := &memDir{live: map[string]*memFile{}, durable: map[string]*memFile{}}
		for name, f := range d.durable {
			c, ok := copies[f]
			if !ok {
				c = &memFile{data: append([]byte(nil), f.durable...), durable: append([]byte(nil), f.durable...), mtime: f.mtime}
				copies[f] = c
			}
			nd.live[name], nd.durable[name] = c, c
		}
		out.dirs[path] = nd
	}
	return out
}

func (m *memFS) split(path string) (*memDir, string, error) {
	d, ok := m.dirs[filepath.Dir(path)]
	if !ok {
		return nil, "", fs.ErrNotExist
	}
	return d, filepath.Base(path), nil
}

func (m *memFS) MkdirAll(dir string, _ os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.step("mkdir"); err != nil {
		return err
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, ok := m.dirs[d]; !ok {
			m.dirs[d] = &memDir{live: map[string]*memFile{}, durable: map[string]*memFile{}}
		}
		if d == filepath.Dir(d) {
			return nil
		}
	}
}

func (m *memFS) ReadDir(dir string) ([]fileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.dirs[filepath.Clean(dir)]
	if !ok {
		return nil, fs.ErrNotExist
	}
	var out []fileInfo
	for name, f := range d.live {
		out = append(out, fileInfo{Name: name, Size: int64(len(f.data)), ModTime: f.mtime})
	}
	for path := range m.dirs {
		if filepath.Dir(path) == filepath.Clean(dir) && path != filepath.Clean(dir) {
			out = append(out, fileInfo{Name: filepath.Base(path), Dir: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memFS) ReadFile(path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, name, err := m.split(path)
	if err != nil {
		return nil, err
	}
	f, ok := d.live[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), f.data...), nil
}

type memHandle struct {
	m      *memFS
	f      *memFile
	closed bool
}

func (m *memFS) CreateExcl(path string, _ os.FileMode) (wfile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.step("create"); err != nil {
		return nil, err
	}
	d, name, err := m.split(path)
	if err != nil {
		return nil, err
	}
	if _, ok := d.live[name]; ok {
		return nil, fs.ErrExist
	}
	f := &memFile{mtime: m.now()}
	d.live[name] = f
	return &memHandle{m: m, f: f}, nil
}

func (h *memHandle) Write(p []byte) (int, error) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if err := h.m.step("write"); err != nil {
		// A failed write may leave part of the data (in the page cache).
		h.f.data = append(h.f.data, p[:len(p)/2]...)
		return len(p) / 2, err
	}
	h.f.data = append(h.f.data, p...)
	return len(p), nil
}

func (h *memHandle) Sync() error {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if err := h.m.step("fsync"); err != nil {
		return err
	}
	h.f.durable = append([]byte(nil), h.f.data...)
	return nil
}

func (h *memHandle) Close() error {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if h.closed {
		return fs.ErrClosed
	}
	h.closed = true
	return h.m.step("close")
}

func (m *memFS) Rename(oldpath, newpath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.step("rename"); err != nil {
		return err
	}
	od, oname, err := m.split(oldpath)
	if err != nil {
		return err
	}
	nd, nname, err := m.split(newpath)
	if err != nil {
		return err
	}
	f, ok := od.live[oname]
	if !ok {
		return fs.ErrNotExist
	}
	delete(od.live, oname)
	nd.live[nname] = f
	return nil
}

func (m *memFS) Remove(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.step("remove"); err != nil {
		return err
	}
	d, name, err := m.split(path)
	if err != nil {
		return err
	}
	if _, ok := d.live[name]; !ok {
		return fs.ErrNotExist
	}
	delete(d.live, name)
	return nil
}

func (m *memFS) SyncDir(dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.step("syncdir"); err != nil {
		return err
	}
	d, ok := m.dirs[filepath.Clean(dir)]
	if !ok {
		return fs.ErrNotExist
	}
	d.durable = map[string]*memFile{}
	for k, v := range d.live {
		d.durable[k] = v
	}
	return nil
}

// names lists the files (not directories) of dir, live view.
func (m *memFS) names(dir string) []string {
	fis, _ := m.ReadDir(dir)
	var out []string
	for _, fi := range fis {
		if !fi.Dir {
			out = append(out, fi.Name)
		}
	}
	return out
}

// put writes a file directly and durably (test setup, not counted).
func (m *memFS) put(path string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dirs[filepath.Dir(path)]
	f := &memFile{data: append([]byte(nil), data...), durable: append([]byte(nil), data...), mtime: m.now()}
	d.live[filepath.Base(path)] = f
	d.durable[filepath.Base(path)] = f
}
