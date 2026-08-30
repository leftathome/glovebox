#!/usr/bin/env bash
# Build the GitHub release body for a version, and enforce that a release
# carrying a breaking change says so where somebody will actually see it.
#
# release.yml used to inline a one-line awk that copied the CHANGELOG's
# `## [X.Y.Z]` section verbatim into the release body. That is the right
# source -- the changelog is where the work is described -- but it produced
# a body with no ordering of its own. v0.8.0 came out at ~68,000 characters
# with the word BREAKING appearing exactly once, 87% of the way down, and
# no link to docs/upgrading.md at all. The port move that breaks every
# archive caller was in there. Nobody skimming that page finds it.
#
# So: a version section may carry an authored `### Upgrade notes`
# subsection. When present it is hoisted to the very top of the release
# body, above `### Added`, followed by the operator checklist link and a
# rule. The rest of the section follows unchanged, with the subsection
# removed so it does not appear twice.
#
# Hoisting is authored, not extracted. The obvious cheaper design is to
# grep the section for lines containing BREAKING and lift those. It does
# not work: the v0.8.0 marker reads "**BREAKING: it now defaults to
# `9093`, so the split is on.**", where "it" is `ingest.bearer_port` from
# the parent bullet. Lifted out of its nesting the sentence loses its
# subject and the callout is worse than nothing. A human writes the
# summary; this script only decides where it goes.
#
# `check` mode is what makes the convention hold. A section containing a
# BREAKING marker and no `### Upgrade notes` fails, and CI runs it on every
# PR -- so the omission is caught while the changelog entry is being
# written, not at `git push --tags` when the release is already going out.
#
# Usage:
#   scripts/release-notes.sh <tag>   emit the release body on stdout
#   scripts/release-notes.sh check   validate every version section
#   scripts/release-notes.sh selftest   exercise the guard against fixtures
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

changelog=${CHANGELOG_FILE:-CHANGELOG.md}
upgrade_guide=docs/upgrading.md
repo_url=https://github.com/leftathome/glovebox

# GitHub rejects a release body over 125,000 characters. The v0.8.0 body
# was already 68,000 and the changelog only grows, so truncate rather than
# let a release fail at tag time -- the header is the part that must
# survive, and the tail is the part a reader can follow a link for.
max_body=125000

if [[ ! -f $changelog ]]; then
	echo "ERROR: $changelog is missing." >&2
	exit 1
fi

# Everything between `## [VERSION]` and the next `## [` heading.
section_for() {
	awk -v ver="$1" '
		index($0, "## [" ver "]") == 1 { found = 1; next }
		/^## \[/ { if (found) exit }
		found { print }
	' "$changelog"
}

# The authored `### Upgrade notes` body, without its heading.
upgrade_notes_of() {
	awk '
		/^### +Upgrade notes[[:space:]]*$/ { found = 1; next }
		/^### / { if (found) exit }
		found { print }
	'
}

# The same text with the `### Upgrade notes` subsection removed.
without_upgrade_notes() {
	awk '
		/^### +Upgrade notes[[:space:]]*$/ { skip = 1; next }
		/^### / { skip = 0 }
		!skip { print }
	'
}

# Strip leading and trailing blank lines.
trim_blank() {
	awk 'NF { seen = 1 } seen' | tac | awk 'NF { seen = 1 } seen' | tac
}

blank() {
	[[ -z ${1//[[:space:]]/} ]]
}

versions() {
	sed -n 's/^## \[\([0-9][^]]*\)\].*/\1/p' "$changelog"
}

missing_notes_message() {
	local v=$1
	echo "ERROR: CHANGELOG section [$v] has a BREAKING marker but no '### Upgrade notes' subsection." >&2
	echo "A breaking change buried in a 60k-character release body is a change nobody reads." >&2
	echo "Add this as the first subsection of [$v]:" >&2
	echo >&2
	echo "    ### Upgrade notes" >&2
	echo >&2
	echo "    <what breaks, who it breaks, and what to do about it -- in prose that" >&2
	echo "     reads on its own, without the surrounding bullets for context>" >&2
	echo >&2
}

if [[ ${1:-} == selftest ]]; then
	# The guard is the point of this script, so it is worth proving that it
	# fires. Each case builds a throwaway CHANGELOG and runs this script
	# against it via CHANGELOG_FILE.
	self=$repo_root/scripts/release-notes.sh
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	cases=0
	bad=0

	ok() { cases=$((cases + 1)); echo "  ok   $1"; }
	no() { cases=$((cases + 1)); bad=$((bad + 1)); echo "  FAIL $1" >&2; }
	try() { CHANGELOG_FILE=$1 "$self" "${@:2}" 2>/dev/null; }

	cat >"$tmp/breaking-with-notes.md" <<'EOF'
# Changelog

## [1.0.0] - 2026-01-01

### Upgrade notes

The frobnicator moved to port 9999. Repoint your callers.

### Changed

- **BREAKING: it moved.** Some detail whose subject is the bullet above.

## [0.9.0] - 2025-12-01

### Added

- A thing.
EOF

	if try "$tmp/breaking-with-notes.md" check >/dev/null; then
		ok "check passes when a BREAKING section has upgrade notes"
	else
		no "check passes when a BREAKING section has upgrade notes"
	fi

	out=$(try "$tmp/breaking-with-notes.md" v1.0.0)
	if [[ ${out%%$'\n'*} == "## ⚠️ Read before upgrading" ]]; then
		ok "upgrade notes are hoisted to the first line"
	else
		no "upgrade notes are hoisted to the first line"
	fi
	if [[ $(grep -c '^### Upgrade notes' <<<"$out") -eq 0 ]]; then
		ok "hoisted notes are not repeated in the body"
	else
		no "hoisted notes are not repeated in the body"
	fi
	if grep -q 'Repoint your callers' <<<"$out" && grep -q 'Some detail' <<<"$out"; then
		ok "both the notes and the rest of the section survive"
	else
		no "both the notes and the rest of the section survive"
	fi
	if grep -q 'blob/v1.0.0/docs/upgrading.md' <<<"$out"; then
		ok "the operator checklist link carries the tag"
	else
		no "the operator checklist link carries the tag"
	fi

	sed '/^### Upgrade notes$/,/^### Changed$/{/^### Changed$/!d}' \
		"$tmp/breaking-with-notes.md" >"$tmp/breaking-no-notes.md"
	if try "$tmp/breaking-no-notes.md" check >/dev/null; then
		no "check fails when a BREAKING section has no upgrade notes"
	else
		ok "check fails when a BREAKING section has no upgrade notes"
	fi
	if try "$tmp/breaking-no-notes.md" v1.0.0 >/dev/null; then
		no "emit refuses a BREAKING section with no upgrade notes"
	else
		ok "emit refuses a BREAKING section with no upgrade notes"
	fi

	if try "$tmp/breaking-with-notes.md" v0.9.0 | grep -q 'Read before upgrading'; then
		no "a section without upgrade notes gets no header"
	else
		ok "a section without upgrade notes gets no header"
	fi

	if try "$tmp/breaking-with-notes.md" v7.7.7 >/dev/null; then
		no "emit refuses a version with no changelog section"
	else
		ok "emit refuses a version with no changelog section"
	fi

	printf '# Changelog\n\n## [2.0.0] - 2026-02-02\n\n## [1.0.0] - 2026-01-01\n\n- A thing.\n' \
		>"$tmp/empty-section.md"
	if try "$tmp/empty-section.md" check >/dev/null; then
		no "check fails on an empty version section"
	else
		ok "check fails on an empty version section"
	fi

	{
		printf '# Changelog\n\n## [3.0.0] - 2026-03-03\n\n### Upgrade notes\n\nShort.\n\n### Added\n\n'
		for _ in $(seq 3000); do
			printf -- '- A bullet long enough to push this section past the release body limit.\n'
		done
	} >"$tmp/huge.md"
	huge=$(try "$tmp/huge.md" v3.0.0)
	if (( ${#huge} <= max_body )) && grep -q 'Release notes truncated' <<<"$huge"; then
		ok "an oversized body is truncated with a pointer to the changelog"
	else
		no "an oversized body is truncated with a pointer to the changelog"
	fi
	if [[ ${huge%%$'\n'*} == "## ⚠️ Read before upgrading" ]]; then
		ok "truncation keeps the header"
	else
		no "truncation keeps the header"
	fi

	echo
	if (( bad )); then
		echo "release-notes selftest: $bad of $cases cases failed." >&2
		exit 1
	fi
	echo "release-notes selftest: $cases cases passed."
	exit 0
fi

if [[ ${1:-} == check ]]; then
	failed=0
	found_any=0
	while read -r v; do
		[[ -n $v ]] || continue
		found_any=1
		section=$(section_for "$v")
		if blank "$section"; then
			echo "ERROR: CHANGELOG section for [$v] is empty; a release cut at $v would publish an empty body." >&2
			failed=1
			continue
		fi
		if grep -q BREAKING <<<"$section" && blank "$(upgrade_notes_of <<<"$section")"; then
			missing_notes_message "$v"
			failed=1
		fi
	done < <(versions)

	if (( ! found_any )); then
		echo "ERROR: $changelog has no '## [X.Y.Z]' version sections." >&2
		exit 1
	fi
	(( failed )) && exit 1
	echo "release notes: every CHANGELOG version section is well-formed."
	exit 0
fi

tag=${1:-}
if [[ -z $tag ]]; then
	echo "usage: scripts/release-notes.sh <tag>|check|selftest" >&2
	exit 2
fi
version=${tag#v}

section=$(section_for "$version")
if blank "$section"; then
	echo "ERROR: no CHANGELOG section found for version $version (tag $tag)." >&2
	echo "The release body comes from '## [$version]' in $changelog; add it before tagging." >&2
	exit 1
fi

notes=$(upgrade_notes_of <<<"$section" | trim_blank)
rest=$(without_upgrade_notes <<<"$section" | trim_blank)

header=""
if ! blank "$notes"; then
	header="## ⚠️ Read before upgrading"$'\n\n'"$notes"$'\n\n'
	if [[ -f $upgrade_guide ]]; then
		header+="**Full operator checklist:** [${upgrade_guide}](${repo_url}/blob/${tag}/${upgrade_guide})"$'\n\n'
	fi
	header+="---"$'\n\n'
elif grep -q BREAKING <<<"$section"; then
	# check mode runs in CI, so reaching here means the guard was bypassed.
	# Fail rather than publish the thing the guard exists to prevent.
	missing_notes_message "$version"
	exit 1
fi

body="${header}${rest}"

if (( ${#body} > max_body )); then
	pointer=$'\n\n---\n\n'"*Release notes truncated at ${max_body} characters. "
	pointer+="Full changelog: [CHANGELOG.md](${repo_url}/blob/${tag}/CHANGELOG.md)*"
	keep=$(( max_body - ${#pointer} ))
	body="${body:0:$keep}${pointer}"
	echo "NOTE: release body exceeded ${max_body} characters; truncated with a link to the full changelog." >&2
fi

printf '%s\n' "$body"
