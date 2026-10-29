package records

import (
	"os"
	"sort"
	"time"
)

// fsys is every file operation the spool makes. The spool writes through
// it in a fixed order (temp file, write, fsync, close, rename, directory
// fsync) so that a crash at any point leaves either the old state or the
// new one. Production uses osFS; the crash tests inject a model that keeps
// only what was fsynced (fs_mem_test.go).
type fsys interface {
	MkdirAll(dir string, perm os.FileMode) error
	ReadDir(dir string) ([]fileInfo, error) // sorted by name
	ReadFile(path string) ([]byte, error)
	CreateExcl(path string, perm os.FileMode) (wfile, error)
	Rename(oldpath, newpath string) error
	Remove(path string) error
	SyncDir(dir string) error
}

type wfile interface {
	Write(p []byte) (int, error)
	Sync() error
	Close() error
}

type fileInfo struct {
	Name    string
	Dir     bool
	Size    int64
	ModTime time.Time
}

type osFS struct{}

func (osFS) MkdirAll(dir string, perm os.FileMode) error { return os.MkdirAll(dir, perm) }

func (osFS) ReadDir(dir string) ([]fileInfo, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]fileInfo, 0, len(des))
	for _, de := range des {
		fi := fileInfo{Name: de.Name(), Dir: de.IsDir()}
		if info, err := de.Info(); err == nil {
			fi.Size, fi.ModTime = info.Size(), info.ModTime()
		}
		out = append(out, fi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (osFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (osFS) CreateExcl(path string, perm os.FileMode) (wfile, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

func (osFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

func (osFS) Remove(path string) error { return os.Remove(path) }

// SyncDir fsyncs a directory, which makes the entries created, renamed or
// removed in it durable.
func (osFS) SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
