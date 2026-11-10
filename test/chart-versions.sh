#!/usr/bin/env bash
# Renders the CSI driver's Helm chart as each Kubernetes version would see it, and checks that it picks the sidecars that
# version needs: the provisioner and resizer before 1.34 are older releases. Then that changed block tracking is off
# unless asked for, and refused where it can't work. Run by "make lint", with the Helm image.
set -u
helm_image=$1
root=$(cd "$(dirname "$0")/.." && pwd)
fails=0

for want in "1.31.0 csi-provisioner:v5.3.0 csi-resizer:v1.14.0" "1.33.5 csi-provisioner:v5.3.0 csi-resizer:v1.14.0" \
	"1.34.0 csi-provisioner:v6.4.0 csi-resizer:v2.3.0" "1.37.2 csi-provisioner:v6.4.0 csi-resizer:v2.3.0"; do
	read -r version provisioner resizer <<<"$want"
	out=$(podman run --rm --pull=missing -v "$root/csi/deploy/helm:/charts:ro,Z" --workdir /charts "$helm_image" \
		template t nvweaver-csi --set cluster=lint --set nvweaver.existingSecret=api --kube-version "$version") || {
		echo "Kubernetes $version: the chart didn't render"
		fails=1
		continue
	}
	for image in "$provisioner" "$resizer"; do
		if ! grep -q "image: registry.k8s.io/sig-storage/$image\$" <<<"$out"; then
			echo "Kubernetes $version: the chart didn't pick $image"
			fails=1
		fi
	done
done

render() { # KUBE-VERSION [helm args]: the chart, rendered; fails as helm does
	local version=$1
	shift
	podman run --rm --pull=missing -v "$root/csi/deploy/helm:/charts:ro,Z" --workdir /charts "$helm_image" \
		template t nvweaver-csi --set cluster=lint --set nvweaver.existingSecret=api --kube-version "$version" "$@" 2>&1
}
expect() { # WHAT CONDITION: report it if the condition fails
	if ! eval "$2"; then
		echo "$1"
		fails=1
	fi
}
crd=(--api-versions cbt.storage.k8s.io/v1beta1/SnapshotMetadataService)

out=$(render 1.37.2 "${crd[@]}")
expect "changed block tracking rendered without being asked for" '! grep -q -e SnapshotMetadataService -e snapshot-metadata <<<"$out"'
out=$(render 1.34.0 "${crd[@]}" --set snapshotMetadata.enabled=true)
if [ $? != 0 ]; then
	echo "changed block tracking, asked for on 1.34: didn't render: $out"
	fails=1
fi
for want in "kind: SnapshotMetadataService" "- --snapshot-metadata" "image: registry.k8s.io/sig-storage/csi-snapshot-metadata:" \
	"kind: Secret"; do
	expect "changed block tracking, asked for on 1.34: no \"$want\"" 'grep -q -F -e "$want" <<<"$out"'
done
out=$(render 1.31.0 "${crd[@]}" --set snapshotMetadata.enabled=true)
expect "changed block tracking on 1.31 wasn't refused for its version: $out" 'grep -q "needs Kubernetes 1.33" <<<"$out"'
out=$(render 1.34.0 --set snapshotMetadata.enabled=true)
expect "changed block tracking without its CRD wasn't refused: $out" 'grep -q "needs the SnapshotMetadataService CRD" <<<"$out"'
out=$(render 1.34.0 "${crd[@]}" --set snapshotMetadata.enabled=true --set snapshotMetadata.tls.existingSecret=mine)
expect "an existing TLS secret with no CA wasn't refused" 'grep -q "caCert is needed" <<<"$out"'

exit $fails
