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
#   scripts/build-targets.sh check-docs    # the doc table matches the build
#   scripts/build-targets.sh images-markdown  # the published-image doc table
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

# images_markdown renders the table docs/deployment.md publishes, so the
# documented image list cannot fall behind the built one the way the
# hand-written table did (it named 10 of 28 components). The component
# directory doubles as the row label: it is the component's real identity
# and tells a reader where the source lives, and unlike a prose name
# ("Google Calendar connector") it cannot be derived wrongly.
images_markdown() {
	printf '| Component | Registry path |\n'
	printf '|-----------|---------------|\n'
	images | while IFS=$'\t' read -r image dockerfile; do
		local component=${dockerfile#./}
		component=${component%/Dockerfile}
		[ "$component" != "Dockerfile" ] || component="." # the scanner itself
		printf '| `%s` | `%s/%s` |\n' "$component" "ghcr.io/leftathome" "$image"
	done
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

# check_docs holds docs/deployment.md's published-image table to the same
# standard as the build: the table it replaced had named 10 of 28
# components, which is the documentation half of the same drift.
check_docs() {
	local doc=docs/deployment.md
	local documented generated
	documented=$(awk '/<!-- BEGIN generated: published-images -->/{f=1; next} /<!-- END generated: published-images -->/{f=0} f' "$doc")
	generated=$(images_markdown)
	if [ "$documented" != "$generated" ]; then
		echo "ERROR: the published-images table in $doc is stale." >&2
		echo "Run 'scripts/build-targets.sh images-markdown' and replace the block between the generated markers." >&2
		diff <(printf '%s\n' "$documented") <(printf '%s\n' "$generated") >&2 || true
		return 1
	fi
	echo "$doc published-images table matches the built image set."
}

case "${1-}" in
binaries) binaries ;;
images) images ;;
images-json) images_json ;;
images-markdown) images_markdown ;;
check) check ;;
check-docs) check_docs ;;
*)
	echo "usage: $0 {binaries|images|images-json|images-markdown|check|check-docs}" >&2
	exit 2
	;;
esac
