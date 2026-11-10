#!/usr/bin/env bash
# Renders the CSI driver's Helm chart as each Kubernetes version would see it, and checks that it picks the sidecars that
# version needs: the provisioner and resizer before 1.34 are older releases. Run by "make lint", with the Helm image.
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
exit $fails
