package archives

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// ----------------------------------------------------------------------
// Test helpers (file-scoped to untar_test.go for Wave B3; the package
// helpers_test.go is owned by Task B2 per the plan's per-package
// convention).
// ----------------------------------------------------------------------

// entry models a single tar archive entry for the in-memory builder.
type entry struct {
	name     string
	typeflag byte
	mode     int64
	uid      int
	gid      int
	size     int64 // declared header Size; if -1 use len(body)
	body     []byte
	pax      map[string]string
}

// buildTar constructs an in-memory tar archive from the given entries.
// A nil pax map omits PAXRecords entirely; a non-nil empty map sets
// PAXRecords to an empty map (Go's tar writer treats this identically
// to nil). Setting size = -1 derives size from len(body).
func buildTar(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		size := e.size
		if size == -1 {
			size = int64(len(e.body))
		}
		h := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Uid:      e.uid,
			Gid:      e.gid,
			Size:     size,
			Format:   tar.FormatPAX,
		}
		if len(e.pax) > 0 {
			h.PAXRecords = e.pax
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("write header for %q: %v", e.name, err)
		}
		if e.typeflag == tar.TypeReg && len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("write body for %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// freshDest creates a per-test temp destination dir at 0700.
func freshDest(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatalf("chmod dest: %v", err)
	}
	return d
}

// runUntar is a small wrapper that returns (entries, written, err).
func runUntar(t *testing.T, body []byte, cfg UntarConfig) (int, int64, error) {
	t.Helper()
	return Untar(bytes.NewReader(body), cfg)
}

// expectSentinel checks that err wraps sentinel.
func expectSentinel(t *testing.T, err error, sentinel error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %v, got nil", sentinel)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected %v, got %v", sentinel, err)
	}
}

// ----------------------------------------------------------------------
// Rejection tests — one per closed-enum reason in spec §4.7.
// ----------------------------------------------------------------------

func TestUntar_NameAbsolute(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "/etc/passwd", typeflag: tar.TypeReg, size: -1, body: []byte("hi")},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarPathTraversal)
}

func TestUntar_NameTraversal_DotDot(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "a/../b", typeflag: tar.TypeReg, size: -1, body: []byte("hi")},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarPathTraversal)
}

func TestUntar_NameTraversal_WindowsDrive(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "C:/Windows/System32", typeflag: tar.TypeReg, size: -1, body: []byte("hi")},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarPathTraversal)
}

func TestUntar_NameTraversal_DoubleSlash(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "a//b", typeflag: tar.TypeReg, size: -1, body: []byte("hi")},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarPathTraversal)
}

func TestUntar_NameTraversal_TrailingSlashOnReg(t *testing.T) {
	// archive/tar.Writer.WriteHeader refuses to emit a TypeReg entry with
	// a trailing-slash Name (it surfaces "filename may not have trailing
	// slash" at write time), so we synthesize the ustar header directly.
	body := buildRawTar(t, "foo/bar/", tar.TypeReg, 0)
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarPathTraversal)
}

func TestUntar_NameContainsNul(t *testing.T) {
	// Go's archive/tar.Reader stops parsing the ustar Name field at the
	// first NUL byte (per the POSIX format spec), so an embedded NUL is
	// not observable through tar.Reader.Next(). The only path that could
	// surface a NUL in a resolved Name is a pax `path` override, which
	// is rejected at step 1 before name validation runs.
	//
	// The closed-enum `name_contains_nul` rejection in spec §4.7 step 3
	// therefore remains as defense-in-depth in validUntarName for the
	// case where a future tar parser preserves embedded NULs. Verify the
	// validator directly.
	if validUntarName("foo\x00bar") {
		t.Fatalf("expected validUntarName(\"foo\\x00bar\") = false")
	}
}

func TestUntar_NameContainsControl_Newline(t *testing.T) {
	body := buildRawTarWithBadName(t, "foo\nbar")
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarNameInvalid)
}

func TestUntar_NameContainsControl_CarriageReturn(t *testing.T) {
	body := buildRawTarWithBadName(t, "foo\rbar")
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarNameInvalid)
}

func TestUntar_NameContainsControl_Tab(t *testing.T) {
	body := buildRawTarWithBadName(t, "foo\tbar")
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarNameInvalid)
}

func TestUntar_NameInvalidUTF8(t *testing.T) {
	// 0xFF is invalid UTF-8 on its own.
	body := buildRawTarWithBadName(t, "foo\xffbar")
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarNameInvalid)
}

func TestUntar_NameTooLong_TotalLength(t *testing.T) {
	// Name > 4096 bytes total. Build via pax `path` record... but pax
	// path overrides are rejected outright. So we need a tar format
	// that can carry a >100-byte name without pax. GNU long-name uses
	// TypeGNULongName which is rejected by typeflag allow-list. PAX
	// without `path` key cannot extend Name beyond ustar's 100+155
	// limit (~256 bytes), insufficient for our 4097-byte test.
	//
	// Workaround: hand-construct a pax extended header that uses some
	// non-`path` key whose value happens to make tar.Reader merge a
	// huge name. There isn't one; pax merging is keyed on `path`.
	//
	// Fall back to raw tar construction with a long name padded across
	// the 100-byte ustar Name + 155-byte Prefix fields. Maximum
	// reachable name length via plain ustar is ~256 bytes — still well
	// below our 4096 cap. So construct via raw PAX path override AND
	// expect ErrTarNameInvalid... but pax `path` is rejected first.
	//
	// Resolution: directly test validUntarName for this case. The
	// rejection logic for name_too_long is exercised by unit-testing
	// the helper. End-to-end test path requires a tar feature we
	// reject for other reasons; documenting the closed-enum reason
	// label by unit-testing the validator is sufficient.
	long := strings.Repeat("a", untarPathMax+1)
	if validUntarName(long) {
		t.Fatalf("expected validUntarName(<4097 a's>) = false")
	}
}

func TestUntar_NameTooLong_ComponentLength(t *testing.T) {
	// Same reasoning as TestUntar_NameTooLong_TotalLength — verify the
	// component-length cap via the validator. A 256-byte single
	// component cannot be expressed in a ustar header (Name field is
	// 100 bytes); pax `path` is rejected; GNU long-name is rejected.
	comp := strings.Repeat("a", untarNameMax+1)
	if validUntarName(comp) {
		t.Fatalf("expected validUntarName(<256 a's single component>) = false")
	}
	// Also a multi-component name where one component is too long.
	twoComp := "ok/" + comp
	if validUntarName(twoComp) {
		t.Fatalf("expected validUntarName(%q) = false", twoComp)
	}
}

func TestUntar_Typeflag_Symlink(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "link", typeflag: tar.TypeSymlink, size: 0},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarTypeflag)
}

func TestUntar_Typeflag_Hardlink(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "link", typeflag: tar.TypeLink, size: 0},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarTypeflag)
}

func TestUntar_Typeflag_Char(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "dev", typeflag: tar.TypeChar, size: 0},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarTypeflag)
}

func TestUntar_Typeflag_Block(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "dev", typeflag: tar.TypeBlock, size: 0},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarTypeflag)
}

func TestUntar_Typeflag_Fifo(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "fifo", typeflag: tar.TypeFifo, size: 0},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarTypeflag)
}

// Go's archive/tar pre-processes TypeGNUSparse, TypeGNULongName, and
// TypeGNULongLink internally — sparse entries are reconstructed into a
// regular file stream and long-name/long-link headers are consumed to
// extend the next entry's Name/Linkname. The typeflag bytes are never
// surfaced to callers of tar.Reader.Next(). Verify validateTypeflag
// rejects each one directly so the defense-in-depth holds if Go's
// behavior ever changes.
func TestUntar_Typeflag_GNUSparse(t *testing.T) {
	if err := validateTypeflag(tar.TypeGNUSparse); !errors.Is(err, ErrTarTypeflag) {
		t.Fatalf("expected ErrTarTypeflag for TypeGNUSparse, got %v", err)
	}
}

func TestUntar_Typeflag_GNULongName(t *testing.T) {
	if err := validateTypeflag(tar.TypeGNULongName); !errors.Is(err, ErrTarTypeflag) {
		t.Fatalf("expected ErrTarTypeflag for TypeGNULongName, got %v", err)
	}
}

func TestUntar_Typeflag_GNULongLink(t *testing.T) {
	if err := validateTypeflag(tar.TypeGNULongLink); !errors.Is(err, ErrTarTypeflag) {
		t.Fatalf("expected ErrTarTypeflag for TypeGNULongLink, got %v", err)
	}
}

func TestUntar_PaxPathOverride_Path(t *testing.T) {
	// Go's archive/tar.Writer only round-trips a pax `path` record back
	// through PAXRecords if Header.Name matches the pax record AND Name
	// is long enough that ustar's 100-byte field can't hold it
	// unaided. Construct that condition: long Name (>100 bytes) with
	// PAXRecords["path"] explicitly set to the same long Name.
	longName := strings.Repeat("a", 120) + "/file.txt"
	body := buildTar(t, []entry{
		{
			name:     longName,
			typeflag: tar.TypeReg,
			size:     -1,
			body:     []byte("hi"),
			pax:      map[string]string{"path": longName},
		},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarPaxOverride)
}

func TestUntar_PaxPathOverride_Linkpath(t *testing.T) {
	// Go's archive/tar.Writer only round-trips a pax `linkpath` record
	// back through PAXRecords when Header.Linkname matches the pax
	// record value. Use a dedicated builder that sets Linkname.
	body := buildTarWithLinkname(t, "innocent.txt", "../etc/passwd",
		map[string]string{"linkpath": "../etc/passwd"})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarPaxOverride)
}

// buildTarWithLinkname constructs a single-entry tar where the entry
// carries a non-empty Linkname plus an explicit pax `linkpath` record.
// Used by TestUntar_PaxPathOverride_Linkpath to ensure Go's tar.Writer
// emits the pax extended header (it suppresses pax records that match
// the default empty-string value).
func buildTarWithLinkname(t *testing.T, name, linkname string, pax map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	h := &tar.Header{
		Name:       name,
		Linkname:   linkname,
		Typeflag:   tar.TypeReg,
		Mode:       0o600,
		Size:       2,
		Format:     tar.FormatPAX,
		PAXRecords: pax,
	}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte("hi")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

func TestUntar_SizeTooLarge(t *testing.T) {
	// Entry declared Size > UploadLength.
	body := buildTar(t, []entry{
		{name: "big.bin", typeflag: tar.TypeReg, size: 2048, body: bytes.Repeat([]byte("a"), 2048)},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 1024})
	expectSentinel(t, err, ErrTarTooLarge)
}

func TestUntar_CumulativeSizeTooLarge(t *testing.T) {
	// UploadLength = 100 → cumulative cap = 200. Build three entries of
	// 80 bytes each (3 * 80 = 240 > 200). Each per-entry Size = 80 <= 100
	// so per-entry cap passes; cumulative cap fires on entry 3 during
	// writeCapped's probe.
	payload := bytes.Repeat([]byte("x"), 80)
	body := buildTar(t, []entry{
		{name: "a.bin", typeflag: tar.TypeReg, size: -1, body: payload},
		{name: "b.bin", typeflag: tar.TypeReg, size: -1, body: payload},
		{name: "c.bin", typeflag: tar.TypeReg, size: -1, body: payload},
	})
	_, _, err := runUntar(t, body, UntarConfig{DestDir: freshDest(t), UploadLength: 100})
	expectSentinel(t, err, ErrTarTooLarge)
}

func TestUntar_EntryCountTooLarge(t *testing.T) {
	// Use MaxEntries=5; build 6 entries; expect ErrTarTooMany on the 6th.
	entries := []entry{}
	for i := 0; i < 6; i++ {
		entries = append(entries, entry{
			name:     "f" + string(rune('0'+i)) + ".txt",
			typeflag: tar.TypeReg,
			size:     -1,
			body:     []byte("x"),
		})
	}
	body := buildTar(t, entries)
	n, _, err := runUntar(t, body, UntarConfig{
		DestDir:      freshDest(t),
		UploadLength: 1024,
		MaxEntries:   5,
	})
	expectSentinel(t, err, ErrTarTooMany)
	if n != 5 {
		t.Fatalf("expected 5 entries extracted before cap, got %d", n)
	}
}

// ----------------------------------------------------------------------
// Mode / UID-GID hygiene tests.
// ----------------------------------------------------------------------

func TestUntar_ModeHygiene_RegularFile(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "file.txt", typeflag: tar.TypeReg, mode: 0o777, size: -1, body: []byte("hi")},
	})
	dest := freshDest(t)
	n, _, err := runUntar(t, body, UntarConfig{DestDir: dest, UploadLength: 1024})
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 entry, got %d", n)
	}
	info, err := os.Stat(filepath.Join(dest, "file.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %o", info.Mode().Perm())
	}
}

func TestUntar_ModeHygiene_Directory(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "subdir/", typeflag: tar.TypeDir, mode: 0o777, size: 0},
	})
	dest := freshDest(t)
	n, _, err := runUntar(t, body, UntarConfig{DestDir: dest, UploadLength: 1024})
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 entry, got %d", n)
	}
	info, err := os.Stat(filepath.Join(dest, "subdir"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected directory")
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("expected mode 0700, got %o", info.Mode().Perm())
	}
}

func TestUntar_ModeHygiene_SetuidBitDropped(t *testing.T) {
	// tar Mode field with set-uid bit (04000) ORed onto 0o755.
	body := buildTar(t, []entry{
		{name: "suid.bin", typeflag: tar.TypeReg, mode: 0o4755, size: -1, body: []byte("hi")},
	})
	dest := freshDest(t)
	_, _, err := runUntar(t, body, UntarConfig{DestDir: dest, UploadLength: 1024})
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "suid.bin"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Exact mode: 0600 with NO set-uid, set-gid, sticky, or any other bit.
	if info.Mode() != 0o600 {
		t.Fatalf("expected exact mode 0600, got %v", info.Mode())
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("set-uid bit not dropped")
	}
	if info.Mode()&os.ModeSetgid != 0 {
		t.Fatalf("set-gid bit not dropped")
	}
	if info.Mode()&os.ModeSticky != 0 {
		t.Fatalf("sticky bit not dropped")
	}
}

func TestUntar_UIDGIDHygiene(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "owned.txt", typeflag: tar.TypeReg, uid: 12345, gid: 12345, size: -1, body: []byte("hi")},
	})
	dest := freshDest(t)
	_, _, err := runUntar(t, body, UntarConfig{DestDir: dest, UploadLength: 1024})
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "owned.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("syscall.Stat_t not available on this platform")
	}
	if int(st.Uid) != os.Getuid() {
		t.Fatalf("expected uid %d (process), got %d", os.Getuid(), st.Uid)
	}
	if int(st.Gid) != os.Getgid() {
		t.Fatalf("expected gid %d (process), got %d", os.Getgid(), st.Gid)
	}
}

// ----------------------------------------------------------------------
// Happy-path tests.
// ----------------------------------------------------------------------

func TestUntar_Happy_ThreeRegularFiles(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "a.txt", typeflag: tar.TypeReg, size: -1, body: []byte("aaa")},
		{name: "b.txt", typeflag: tar.TypeReg, size: -1, body: []byte("bbbb")},
		{name: "c.txt", typeflag: tar.TypeReg, size: -1, body: []byte("ccccc")},
	})
	dest := freshDest(t)
	n, w, err := runUntar(t, body, UntarConfig{DestDir: dest, UploadLength: 1024})
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	if n != 3 {
		t.Fatalf("entries = %d, want 3", n)
	}
	if w != int64(3+4+5) {
		t.Fatalf("written = %d, want 12", w)
	}
	for name, want := range map[string]string{"a.txt": "aaa", "b.txt": "bbbb", "c.txt": "ccccc"} {
		p := filepath.Join(dest, name)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestUntar_Happy_NestedDirsAndFiles(t *testing.T) {
	body := buildTar(t, []entry{
		{name: "dir1/", typeflag: tar.TypeDir, size: 0},
		{name: "dir1/inner.txt", typeflag: tar.TypeReg, size: -1, body: []byte("inside1")},
		{name: "dir2/", typeflag: tar.TypeDir, size: 0},
		{name: "dir2/nested.txt", typeflag: tar.TypeReg, size: -1, body: []byte("inside2")},
	})
	dest := freshDest(t)
	n, _, err := runUntar(t, body, UntarConfig{DestDir: dest, UploadLength: 1024})
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	if n != 4 {
		t.Fatalf("entries = %d, want 4", n)
	}
	for _, d := range []string{"dir1", "dir2"} {
		info, err := os.Stat(filepath.Join(dest, d))
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s not a directory", d)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", d, info.Mode().Perm())
		}
	}
	for p, want := range map[string]string{
		"dir1/inner.txt":  "inside1",
		"dir2/nested.txt": "inside2",
	} {
		got, err := os.ReadFile(filepath.Join(dest, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", p, got, want)
		}
		info, err := os.Stat(filepath.Join(dest, p))
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", p, info.Mode().Perm())
		}
	}
}

func TestUntar_Happy_EmptyTar(t *testing.T) {
	body := buildTar(t, nil)
	dest := freshDest(t)
	n, w, err := runUntar(t, body, UntarConfig{DestDir: dest, UploadLength: 1024})
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	if n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
	if w != 0 {
		t.Fatalf("written = %d, want 0", w)
	}
}

// ----------------------------------------------------------------------
// Raw tar header builders for cases the standard tar.Writer refuses.
// ----------------------------------------------------------------------

// buildRawTarWithBadName synthesizes a minimal ustar header carrying an
// arbitrary (potentially invalid) Name field. tar.Writer's WriteHeader
// validates names against UTF-8 / NUL constraints, so we hand-construct.
//
// The ustar header layout (POSIX 1003.1-1988):
//
//	name[100]  mode[8]  uid[8]  gid[8]  size[12]  mtime[12]  chksum[8]
//	typeflag[1]  linkname[100]  magic[6]  version[2]  uname[32]  gname[32]
//	devmajor[8]  devminor[8]  prefix[155]  pad[12]
//
// Total: 512 bytes per header block.
func buildRawTarWithBadName(t *testing.T, name string) []byte {
	t.Helper()
	return buildRawTar(t, name, byte(tar.TypeReg), 0)
}

// buildRawTar constructs a raw tar archive with a single entry carrying
// the given name, typeflag, and body size (no body bytes written; size
// must be zero for the parser to advance cleanly to EOF).
func buildRawTar(t *testing.T, name string, typeflag byte, size int64) []byte {
	t.Helper()
	if size != 0 {
		t.Fatalf("buildRawTar: only size=0 supported")
	}
	var header [512]byte

	// Name: bytes 0..99. Truncate to 100 bytes if longer; the goal is to
	// surface the parser-visible name, which is what we want for the
	// safety-rule rejection.
	nameBytes := []byte(name)
	if len(nameBytes) > 100 {
		nameBytes = nameBytes[:100]
	}
	copy(header[0:100], nameBytes)

	// Mode: "0000600\x00" (octal 0600 + space-padded).
	copy(header[100:108], []byte("0000600\x00"))
	// Uid: "0000000\x00".
	copy(header[108:116], []byte("0000000\x00"))
	// Gid: "0000000\x00".
	copy(header[116:124], []byte("0000000\x00"))
	// Size: 12 bytes octal, zero-padded with NUL terminator.
	copy(header[124:136], []byte("00000000000\x00"))
	// Mtime: 12 bytes octal.
	copy(header[136:148], []byte("00000000000\x00"))

	// Checksum field is initialized to 8 spaces for checksum computation.
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}

	header[156] = typeflag

	// Magic + version: "ustar\x0000".
	copy(header[257:263], []byte("ustar\x00"))
	copy(header[263:265], []byte("00"))

	// Compute checksum: unsigned sum of all 512 header bytes, then
	// write as 6-digit octal + NUL + space.
	var sum uint
	for _, b := range header {
		sum += uint(b)
	}
	chk := []byte{}
	chk = appendOctal6(chk, sum)
	chk = append(chk, 0, ' ')
	copy(header[148:156], chk)

	var buf bytes.Buffer
	buf.Write(header[:])
	// Two 512-byte zero blocks terminate a tar archive.
	var zero [1024]byte
	buf.Write(zero[:])
	return buf.Bytes()
}

// appendOctal6 appends a 6-digit zero-padded octal representation of v.
func appendOctal6(dst []byte, v uint) []byte {
	const width = 6
	var digits [22]byte
	n := 0
	if v == 0 {
		digits[n] = '0'
		n++
	} else {
		for v > 0 {
			digits[n] = byte('0' + (v & 7))
			n++
			v >>= 3
		}
	}
	for n < width {
		digits[n] = '0'
		n++
	}
	// Reverse into dst.
	for i := n - 1; i >= 0; i-- {
		dst = append(dst, digits[i])
	}
	return dst
}

// ---- ClassifyReason ----

func TestClassifyReason_MapsKnownSentinels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
		ok   bool
	}{
		{"nil", nil, "", false},
		{"pax override", ErrTarPaxOverride, "pax_path_override", true},
		{"typeflag", ErrTarTypeflag, "typeflag_disallowed", true},
		{"name invalid", ErrTarNameInvalid, "name_invalid", true},
		{"path traversal", ErrTarPathTraversal, "name_traversal", true},
		{"too large", ErrTarTooLarge, "size_too_large", true},
		{"too many", ErrTarTooMany, "entry_count_too_large", true},
		{"unknown", errors.New("disk full"), "", false},
		{"wrapped name invalid", fmt.Errorf("entry %q: %w", "bad", ErrTarNameInvalid), "name_invalid", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ClassifyReason(tc.err)
			if got != tc.want || ok != tc.ok {
				t.Errorf("ClassifyReason(%v) = (%q, %v), want (%q, %v)",
					tc.err, got, ok, tc.want, tc.ok)
			}
		})
	}
}

