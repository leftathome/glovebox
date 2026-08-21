package archives

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fuzz targets for the two parsers in this package that consume
// attacker-supplied bytes: the tar body of an archive upload, and the
// base64 Upload-Metadata header.
//
// These assert invariants, not merely absence of panics. A tar parser that
// survives every input while writing one file outside its destination has
// not passed anything worth testing.
//
// Run the seed corpus with `go test ./internal/ingest/archives/`; run the
// fuzzer with `go test ./internal/ingest/archives/ -run xxx -fuzz FuzzUntar`.

// tarWith builds a small tar archive from name/body pairs, used to seed the
// corpus with shapes the fuzzer would take a long time to discover.
func tarWith(t testing.TB, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range entries {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o600,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	tw.Close()
	return buf.Bytes()
}

// FuzzUntar asserts the safety property that matters: whatever the input,
// nothing is created outside the destination directory.
//
// Untar is the most exposed parser in the service. It runs on the body of a
// multi-GB upload from an authenticated but not necessarily trustworthy
// producer, and its job is precisely to refuse the hostile shapes -- pax
// path overrides, traversal, absolute names, symlinks, device nodes.
func FuzzUntar(f *testing.F) {
	f.Add(tarWith(f, map[string]string{"ocr.txt": "hello"}))
	f.Add(tarWith(f, map[string]string{"a/b/c.txt": "nested"}))
	f.Add([]byte("not a tar at all"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		if err := os.MkdirAll(dest, 0o700); err != nil {
			t.Fatalf("mkdir dest: %v", err)
		}

		// Errors are the expected outcome for most inputs; the contract is
		// about what lands on disk, not about returning nil.
		_, written, _ := Untar(bytes.NewReader(data), UntarConfig{
			DestDir:      dest,
			UploadLength: int64(len(data)) + 1024,
			MaxEntries:   64,
		})

		if written < 0 {
			t.Errorf("negative bytes written: %d", written)
		}

		// Nothing may exist under root except inside dest.
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // races with cleanup are not the property under test
			}
			if path == root || path == dest {
				return nil
			}
			rel, relErr := filepath.Rel(dest, path)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Errorf("Untar created %q outside the destination %q", path, dest)
			}
			// Extracted entries must not be symlinks: a symlink inside the
			// destination is a traversal primitive for whatever reads the
			// tree afterwards.
			if info.Mode()&os.ModeSymlink != 0 {
				t.Errorf("Untar created a symlink at %q", path)
			}
			if !info.IsDir() && info.Mode()&os.ModeType != 0 {
				t.Errorf("Untar created a non-regular file at %q (mode %v)", path, info.Mode())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
	})
}

// FuzzParseUploadMetadata drives the base64 Upload-Metadata header parser.
//
// The invariant is that a parsed archive_id can never be used to escape the
// archive root: it is joined onto a path immediately afterwards.
func FuzzParseUploadMetadata(f *testing.F) {
	f.Add("archive_id c2Nhbi0wMDAx,media_type YXJjaGl2ZS9tYm94", int64(1024))
	f.Add("", int64(0))
	f.Add("archive_id", int64(1))
	f.Add("archive_id !!!!not-base64!!!!", int64(1))
	// Dot-only ids: a fully-valid eight-key header carrying one is far
	// beyond what the mutator will assemble unaided, and it is exactly the
	// case that used to slip through (see TestParseUploadMetadata_
	// RejectsDotOnlyArchiveID). Seed it so the invariant below is enforced
	// rather than merely stated.
	f.Add(validHeader(map[string]string{"archive_id": ".."}), int64(1024))
	f.Add(validHeader(map[string]string{"archive_id": "."}), int64(1024))
	f.Add(validHeader(nil), int64(1024))

	f.Fuzz(func(t *testing.T, header string, uploadLength int64) {
		meta, err := ParseUploadMetadata(header, uploadLength)
		if err != nil {
			return
		}
		if meta == nil {
			t.Fatal("ParseUploadMetadata returned nil metadata with nil error")
		}

		id := meta.ArchiveID
		if id == "" {
			t.Errorf("accepted metadata with an empty archive_id: %q", header)
		}
		if strings.ContainsAny(id, `/\`) {
			t.Errorf("accepted archive_id %q containing a path separator", id)
		}
		if id == "." || id == ".." || strings.Trim(id, ".") == "" {
			t.Errorf("accepted dot-only archive_id %q", id)
		}
		// The decisive check: joining the id under a root must stay under it.
		root := "/srv/archives"
		joined := filepath.Clean(filepath.Join(root, id))
		if !strings.HasPrefix(joined, root+string(filepath.Separator)) {
			t.Errorf("archive_id %q escapes the archive root: %q", id, joined)
		}
	})
}
