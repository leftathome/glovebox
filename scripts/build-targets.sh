#!/usr/bin/env bash
# Single source of truth for what this repository ships.
#
# The set of connectors and importers used to be written out by hand in
# three places -- ci.yml's binary build, ci.yml's container matrix and
# release.yml's archive build -- and all three drifted apart. Release
# archives shipped 10 of 24 connectors and no importers at all while the
# README promised "all connector binaries"; the binary build in CI missed
# the same 14. Everything is discovered here instead, so a new connector
# directory is picked up by every consumer at once.
#
# Usage:
#   scripts/build-targets.sh binaries      # <binary-name>TAB<go package>
#   scripts/build-targets.sh images        # <image-name>TAB<dockerfile>
#   scripts/build-targets.sh images-json   # the same, as a GH Actions matrix
#   scripts/build-targets.sh check         # every component is accounted for
#
# Naming follows what the repository already publishes: the root module is
# `glovebox`, connectors build `<name>-connector` binaries into
# `glovebox-<name>` images, importers build `<name>-importer` binaries into
# `glovebox-<name>-importer` images. `<name>` is the directory under
# connectors/ or importers/, not the Go package directory, so a connector
# that keeps its entrypoint in cmd/ (schoology) still gets its own name.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

# component_name <relative-package-dir> -> the connectors/ or importers/
# directory name that owns it.
component_name() {
	local rel=${1#*/}
	printf '%s' "${rel%%/*}"
}

binaries() {
	printf 'glovebox\t.\n'
	# Only main packages are entrypoints; a connector's library packages and
	# its testdata are not. go list also finds entrypoints nested in cmd/.
	go list -f '{{if eq .Name "main"}}{{.Dir}}{{end}}' ./connectors/... ./importers/... |
		while read -r dir; do
			[ -n "$dir" ] || continue
			local rel=${dir#"$repo_root"/}
			local name
			name=$(component_name "$rel")
			case "$rel" in
			connectors/*) printf '%s-connector\t./%s\n' "$name" "$rel" ;;
			importers/*) printf '%s-importer\t./%s\n' "$name" "$rel" ;;
			esac
		done | sort
}

images() {
	printf 'glovebox\t./Dockerfile\n'
	# A component ships an image exactly when it has a Dockerfile. The
	# enricher-runtime and enrichment-smoke images are not components and
	# are built by their own jobs, so they are deliberately absent here.
	for dockerfile in connectors/*/Dockerfile importers/*/Dockerfile; do
		[ -f "$dockerfile" ] || continue
		local dir name
		dir=$(dirname "$dockerfile")
		name=$(basename "$dir")
		case "$dir" in
		connectors/*) printf 'glovebox-%s\t./%s\n' "$name" "$dockerfile" ;;
		importers/*) printf 'glovebox-%s-importer\t./%s\n' "$name" "$dockerfile" ;;
		esac
	done | sort
}

images_json() {
	images | awk -F'\t' '
		BEGIN { printf "[" }
		{
			if (NR > 1) printf ","
			printf "{\"image\":\"%s\",\"dockerfile\":\"%s\",\"context\":\".\"}", $1, $2
		}
		END { printf "]" }
	'
	printf '\n'
}

# check guards against the failure mode that silently ships nothing: if
# discovery ever returns an empty or partial list, every consumer keeps
# passing while building fewer and fewer components -- the same quiet drift
# the hand-written lists suffered, only harder to spot. Every component
# directory must yield a binary, and every Dockerfile must yield an image.
check() {
	local failures=0 dir name
	local binary_targets image_targets
	binary_targets=$(binaries)
	image_targets=$(images)

	for dir in connectors/*/ importers/*/; do
		name=$(basename "$dir")
		if ! printf '%s\n' "$binary_targets" | grep -q "	\./${dir%/}\(/\|$\)"; then
			echo "no binary target discovered for ${dir%/}" >&2
			failures=$((failures + 1))
		fi
		if [ -f "${dir}Dockerfile" ] &&
			! printf '%s\n' "$image_targets" | grep -q "	\./${dir}Dockerfile$"; then
			echo "no image discovered for ${dir}Dockerfile" >&2
			failures=$((failures + 1))
		fi
	done

	if [ "$failures" -gt 0 ]; then
		echo "build-targets.sh: $failures component(s) unaccounted for" >&2
		return 1
	fi
	echo "build-targets.sh: $(printf '%s\n' "$binary_targets" | wc -l) binaries, $(printf '%s\n' "$image_targets" | wc -l) images, every component accounted for."
}

case "${1-}" in
binaries) binaries ;;
images) images ;;
images-json) images_json ;;
check) check ;;
*)
	echo "usage: $0 {binaries|images|images-json|check}" >&2
	exit 2
	;;
esac
