#!/usr/bin/env bash
# Makes the SBOMs for a release, in SPDX JSON, from the tested images and from build/, and fails if one doesn't list what THIRD_PARTY_NOTICES.md says is in it.
# Usage: make-sboms.sh VERSION ARCH...   Local images are expected as release.yml loads them: localhost/nvweaver:dev for amd64, and :dev-arm64 for arm64.
set -euo pipefail
shopt -s inherit_errexit

SYFT=docker.io/anchore/syft:v1.57.0@sha256:3c70e6d5979364cefdc22b51617a78fd264574d3bd6b4f84a351204147025e2a
VERSION=$1
shift
ROOT=$(cd "$(dirname "$0")/.." && pwd)
NOTICES=${NOTICES:-$ROOT/THIRD_PARTY_NOTICES.md}
OUT=$ROOT/build
WORK=$(mktemp -d)
trap 'rm -rf "${WORK:?}"' EXIT

scan() { # SOURCE: what Syft sees, into $WORK/scanned
	podman run --rm --pull=missing -v "$WORK:/w:Z" -e SYFT_CHECK_FOR_APP_UPDATE=false "$SYFT" "$1" -q -o "spdx-json=/w/scanned"
}

# Go reports no version for nvweaver itself, as it isn't a published module.
finish() { # OUTPUT
	jq --arg v "$VERSION" '(.packages[] | select(.name == "nvweaver")).versionInfo = $v' "$WORK/scanned" >"$1"
}

# SPDK, DPDK and ISA-L are compiled into one binary, so a scanner can't see them, and the SBOM would look clean without the code that matters most.
# The image's build records what it really built from.
add_components() { # FILE COMPONENTS
	local name version commit url purl
	while IFS=$'\t' read -r name version commit url; do
		[ -n "$name" ] && [ -n "$version" ] && [ -n "$commit" ] && [ -n "$url" ] || { echo "a bad line in the image's components: $name" >&2; exit 1; }
		purl=pkg:github/$(sed -E 's|^https://github.com/||; s|\.git$||' <<<"$url")@$commit
		jq --arg name "$name" --arg version "$version" --arg commit "$commit" --arg url "$url" --arg purl "$purl" '
			("SPDXRef-Package-compiled-in-" + ($name | gsub("[^A-Za-z0-9]"; "-"))) as $id
			| .packages += [{
				name: $name, SPDXID: $id, versionInfo: $version, downloadLocation: ("git+" + $url + "@" + $commit),
				filesAnalyzed: false, licenseConcluded: "NOASSERTION", licenseDeclared: "NOASSERTION",
				sourceInfo: ("Compiled into /usr/local/bin/nvmf_tgt, from commit " + $commit),
				externalRefs: [{ referenceCategory: "PACKAGE-MANAGER", referenceType: "purl", referenceLocator: $purl }]
			}]
			| .relationships += [{ spdxElementId: .documentDescribes[0], relatedSpdxElement: $id, relationshipType: "CONTAINS" }]
		' "$1" >"$1.new"
		mv "$1.new" "$1"
	done <"$2"
}

# What each row of the notices is called in an SBOM. A new row with no name here fails, so that nobody can add one and have it go unchecked.
sbom_name() {
	case "$1" in
	"Go runtime and standard library") echo stdlib ;;
	golang.org/x/crypto | SPDK | DPDK | ISA-L) echo "$1" ;;
	*) echo "nothing says what '$1' from the notices is called in an SBOM: add it to $0" >&2; exit 1 ;;
	esac
}

check() { # FILE KIND, where KIND is spdk for the SPDK image, or anything else
	local row component where name version
	while IFS= read -r row; do
		component=$(cut -d'|' -f2 <<<"$row" | sed -E 's/^ *//; s/ *$//; s/^\[([^]]*)\].*/\1/')
		where=$(cut -d'|' -f3 <<<"$row")
		[ "$component" = Component ] && continue
		case "$component" in ---*) continue ;; esac
		case "$where" in *"SPDK image"*) [ "$2" = spdk ] || continue ;; esac
		name=$(sbom_name "$component")
		version=$(jq -r --arg n "$name" '[.packages[] | select(.name == $n) | .versionInfo][0] // ""' "$1")
		case "$version" in
		"" | UNKNOWN | NOASSERTION) echo "$(basename "$1") doesn't list $component ($name) with a version, which THIRD_PARTY_NOTICES.md says is in it" >&2; exit 1 ;;
		esac
		echo "  $(basename "$1"): $name $version"
	done < <(grep '^|' "$NOTICES")
	version=$(jq -r '[.packages[] | select(.name == "nvweaver") | .versionInfo][0] // ""' "$1")
	[ "$version" = "$VERSION" ] || { echo "$(basename "$1") says nvweaver is '$version', not $VERSION" >&2; exit 1; }
}

for arch in "$@"; do
	suffix=
	[ "$arch" = amd64 ] || suffix=-$arch

	podman save -q --format oci-archive -o "$WORK/image.tar" "localhost/nvweaver:dev$suffix"
	scan oci-archive:/w/image.tar
	finish "$OUT/nvweaver-$VERSION-image-$arch.spdx.json"
	check "$OUT/nvweaver-$VERSION-image-$arch.spdx.json" kernel

	podman save -q --format oci-archive -o "$WORK/image.tar" "localhost/nvweaver:spdk$suffix"
	scan oci-archive:/w/image.tar
	finish "$OUT/nvweaver-$VERSION-spdk-image-$arch.spdx.json"
	container=$(podman create --pull=never "localhost/nvweaver:spdk$suffix" 2>/dev/null)
	podman cp "$container:/usr/share/licenses/nvweaver/spdk-components.tsv" "$WORK/components"
	podman rm -f "$container" >/dev/null
	add_components "$OUT/nvweaver-$VERSION-spdk-image-$arch.spdx.json" "$WORK/components"
	check "$OUT/nvweaver-$VERSION-spdk-image-$arch.spdx.json" spdk

	mkdir -p "$WORK/$arch"
	tar xzf "$OUT/nvweaver-$VERSION-linux-$arch.tar.gz" -C "$WORK/$arch"
	scan "dir:/w/$arch"
	finish "$OUT/nvweaver-$VERSION-linux-$arch.spdx.json"
	check "$OUT/nvweaver-$VERSION-linux-$arch.spdx.json" tarball

	cp "$OUT/nvweaver-cli-$VERSION-linux-$arch" "$WORK/nvweaver-$arch"
	scan "file:/w/nvweaver-$arch"
	finish "$OUT/nvweaver-cli-$VERSION-linux-$arch.spdx.json"
	check "$OUT/nvweaver-cli-$VERSION-linux-$arch.spdx.json" cli
done

# Everything should be built with one Go. The images use the newest 1.26, and setup-go once followed the go line in go.mod instead, which shipped binaries that were
# ten patch releases behind, with any security fixes in those.
go_versions=$(jq -r '.packages[] | select(.name == "stdlib") | .versionInfo' "$OUT"/*.spdx.json | sort -u)
if [ "$(wc -l <<<"$go_versions")" != 1 ]; then
	echo "these were built with different versions of Go:" $go_versions >&2
	exit 1
fi
echo "  all built with $go_versions"
