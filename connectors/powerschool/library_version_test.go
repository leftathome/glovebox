package powerschool

import (
	"errors"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// TestReviewedLibraryVersion is the CI half of the version pin: go.mod and
// reviewedLibraryVersion must name the same powerschool-go release, and
// go.mod must not replace the module. Raising the requirement without
// reviewing the library and raising the constant fails here.
func TestReviewedLibraryVersion(t *testing.T) {
	raw, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	gomod := string(raw)

	require := regexp.MustCompile(`(?m)^\s*(?:require\s+)?`+regexp.QuoteMeta(libraryModule)+`\s+(\S+)`).FindAllStringSubmatch(gomod, -1)
	if len(require) != 1 {
		t.Fatalf("go.mod requires %s %d times, want once", libraryModule, len(require))
	}
	if got := require[0][1]; got != reviewedLibraryVersion {
		t.Errorf("go.mod requires %s %s but the reviewed version is %s.\n"+
			"Read the library diff between the two for new requests, POST paths and form fields, "+
			"then change reviewedLibraryVersion (transport.go) in the same commit.", libraryModule, got, reviewedLibraryVersion)
	}
	if regexp.MustCompile(`(?m)^\s*(?:replace\s+)?` + regexp.QuoteMeta(libraryModule) + `(\s+\S+)?\s*=>`).MatchString(gomod) {
		t.Errorf("go.mod replaces %s; the connector only runs on the reviewed release", libraryModule)
	}

	// And the binary under test: `go test` records the module graph, so
	// this is the same check main.go runs at startup, on real build
	// information.
	if err := CheckLibrary(); err != nil {
		t.Errorf("CheckLibrary on this test binary: %v", err)
	}
	if got := LibraryVersion(); got != reviewedLibraryVersion {
		t.Errorf("linked library version = %q, want %q", got, reviewedLibraryVersion)
	}
}

func TestCheckLibraryVersion(t *testing.T) {
	lib := func(version string, replace *debug.Module) *debug.BuildInfo {
		return &debug.BuildInfo{Deps: []*debug.Module{
			nil,
			{Path: "github.com/google/uuid", Version: reviewedLibraryVersion},
			{Path: libraryModule, Version: version, Replace: replace},
		}}
	}
	cases := []struct {
		name string
		bi   *debug.BuildInfo
		ok   bool
		want string // substring of the error; "" means accepted
	}{
		{"the reviewed release", lib(reviewedLibraryVersion, nil), true, ""},
		{"no build information", nil, false, "no build information"},
		{"build information reported present but nil", nil, true, "no build information"},
		{"library not listed", &debug.BuildInfo{Deps: []*debug.Module{{Path: "github.com/google/uuid", Version: "v1.6.0"}}}, true, "does not list"},
		{"no dependencies at all", &debug.BuildInfo{}, true, "does not list"},
		{"the previous release", lib("v0.3.0", nil), true, `linked version is "v0.3.0"`},
		{"a newer release", lib("v0.4.1", nil), true, `linked version is "v0.4.1"`},
		{"a pseudo-version", lib("v0.4.1-0.20261001000000-abcdefabcdef", nil), true, "linked version is"},
		{"devel", lib("(devel)", nil), true, "linked version is"},
		{"empty version", lib("", nil), true, "linked version is"},
		{"build metadata on the reviewed version", lib(reviewedLibraryVersion+"+dirty", nil), true, "linked version is"},
		{"replaced by a directory", lib(reviewedLibraryVersion, &debug.Module{Path: "../powerschool-go"}), true, "replaces the module"},
		{"replaced by a fork at the same version", lib(reviewedLibraryVersion,
			&debug.Module{Path: "gitlab.example/x/powerschool-go", Version: reviewedLibraryVersion}), true, "replaces the module"},
		{"replaced by itself at the same version", lib(reviewedLibraryVersion,
			&debug.Module{Path: libraryModule, Version: reviewedLibraryVersion}), true, "replaces the module"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkLibraryVersion(tc.bi, tc.ok)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrLibraryVersion) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrLibraryVersion mentioning %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), reviewedLibraryVersion) {
				t.Errorf("err = %v, want it to name the reviewed version", err)
			}
		})
	}
}

// TestCheckLibrary_RefusesWhatItCannotVerify: the startup check itself, not
// only its helper, refuses when the build information is missing or names
// another version. There is no switch that lets it pass.
func TestCheckLibrary_RefusesWhatItCannotVerify(t *testing.T) {
	real := readBuildInfo
	t.Cleanup(func() { readBuildInfo = real })

	readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	if err := CheckLibrary(); !errors.Is(err, ErrLibraryVersion) {
		t.Errorf("no build information: err = %v, want ErrLibraryVersion", err)
	}
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Deps: []*debug.Module{{Path: libraryModule, Version: "v0.3.0"}}}, true
	}
	if err := CheckLibrary(); !errors.Is(err, ErrLibraryVersion) {
		t.Errorf("another version: err = %v, want ErrLibraryVersion", err)
	}
	for _, env := range []string{"POWERSCHOOL_SKIP_LIBRARY_CHECK", "POWERSCHOOL_ALLOW_UNREVIEWED_LIBRARY", "GLOVEBOX_TEST"} {
		t.Setenv(env, "1")
	}
	if err := CheckLibrary(); !errors.Is(err, ErrLibraryVersion) {
		t.Errorf("with would-be bypass variables set: err = %v, want ErrLibraryVersion", err)
	}

	readBuildInfo = real
	if err := CheckLibrary(); err != nil {
		t.Errorf("real build information: %v", err)
	}
}
