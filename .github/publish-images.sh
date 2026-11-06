#!/usr/bin/env bash
# Publishes the kernel and SPDK images for each architecture, from images that are already loaded:
#
#   localhost/nvweaver:dev    localhost/nvweaver:spdk         amd64
#   localhost/nvweaver:dev-arm64  localhost/nvweaver:spdk-arm64   arm64
#
#   publish-images.sh push     Pushes each architecture under its own tag, then a multi-arch tag
#                              over them. Prints kernel=<digest> and spdk=<digest>, of the
#                              multi-arch tags, for $GITHUB_OUTPUT.
#   publish-images.sh latest   Points latest and latest-spdk at what "push" published.
#
# IMAGE and VERSION say where and what, such as ghcr.io/xphyr/nvweaver and 0.2.0.
set -euo pipefail
# Otherwise a failure inside $(...) carries on regardless.
shopt -s inherit_errexit

: "${IMAGE:?}" "${VERSION:?}"

# Before anything is pushed, as check_published needs these afterwards.
command -v skopeo python3 >/dev/null

# Refuses an image of the wrong architecture, which would otherwise publish without complaint and
# only fail for the people who pull it.
check_arch() {
	local local_image=$1 want=$2 got
	got=$(podman image inspect --format '{{.Architecture}}' "$local_image")
	if [ "$got" != "$want" ]; then
		echo "$local_image is $got, not $want" >&2
		return 1
	fi
}

# Reads back what the registry now serves, as this is what people will pull.
check_published() {
	local ref=$1 got
	got=$(skopeo inspect --raw "docker://$ref" | python3 -c '
import json, sys
print(" ".join(sorted(m["platform"]["architecture"] for m in json.load(sys.stdin)["manifests"]
                      if m["platform"]["architecture"] != "unknown")))')
	if [ "$got" != "amd64 arm64" ]; then
		echo "$ref has architectures [$got], not [amd64 arm64]" >&2
		return 1
	fi
}

# publish_tag TAG AMD64_IMAGE ARM64_IMAGE: prints the digest of the multi-arch tag.
publish_tag() {
	local tag=$1 amd64=$2 arm64=$3 list="nvweaver-list:$1" digest
	check_arch "$amd64" amd64
	check_arch "$arm64" arm64
	podman push "$amd64" "docker://$IMAGE:$tag-amd64" >&2
	podman push "$arm64" "docker://$IMAGE:$tag-arm64" >&2
	podman manifest create "$list" >&2
	podman manifest add "$list" "docker://$IMAGE:$tag-amd64" >&2
	podman manifest add "$list" "docker://$IMAGE:$tag-arm64" >&2
	digest=$(mktemp)
	podman manifest push --all --digestfile "$digest" "$list" "docker://$IMAGE:$tag" >&2
	check_published "$IMAGE:$tag"
	cat "$digest"
	rm -f "$digest"
}

case "${1:-}" in
push)
	# Assigned first, as echo would hide a failure.
	kernel=$(publish_tag "$VERSION" localhost/nvweaver:dev localhost/nvweaver:dev-arm64)
	spdk=$(publish_tag "$VERSION-spdk" localhost/nvweaver:spdk localhost/nvweaver:spdk-arm64)
	echo "kernel=$kernel"
	echo "spdk=$spdk"
	;;
latest)
	podman manifest push --all "nvweaver-list:$VERSION" "docker://$IMAGE:latest" >&2
	podman manifest push --all "nvweaver-list:$VERSION-spdk" "docker://$IMAGE:latest-spdk" >&2
	check_published "$IMAGE:latest"
	check_published "$IMAGE:latest-spdk"
	;;
*)
	echo "usage: $0 push|latest" >&2
	exit 2
	;;
esac
