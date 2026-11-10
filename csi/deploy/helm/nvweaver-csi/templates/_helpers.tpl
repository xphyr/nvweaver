{{- define "nvweaver-csi.labels" -}}
app.kubernetes.io/name: nvweaver-csi
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{- define "nvweaver-csi.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{- define "nvweaver-csi.cluster" -}}
{{- $c := required "cluster is required: a name for this cluster, as the scoped token is named" .Values.cluster -}}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$" $c) -}}
{{- fail "cluster must be 1 to 40 lowercase letters, digits and dashes" -}}
{{- end -}}
{{ $c }}
{{- end }}

{{- define "nvweaver-csi.secretName" -}}
{{ .Values.nvweaver.existingSecret | default (printf "%s-api" .Release.Name) }}
{{- end }}

{{- define "nvweaver-csi.driverArgs" -}}
- --endpoint=unix:///csi/csi.sock
- --cluster={{ include "nvweaver-csi.cluster" . }}
- --driver-name={{ .Values.driverName }}
- --nqn-prefix={{ .Values.nqnPrefix }}
{{- end }}

{{- define "nvweaver-csi.snapshotMetadataName" -}}
{{ .Release.Name }}-snapshot-metadata
{{- end }}

{{- /* The audience backup clients ask their tokens for: the driver's name, unless set. */}}
{{- define "nvweaver-csi.snapshotMetadataAudience" -}}
{{ .Values.snapshotMetadata.audience | default .Values.driverName }}
{{- end }}

{{- define "nvweaver-csi.openshift" -}}
{{- $v := toString .Values.openshift.privilegedSCC -}}
{{- if or (eq $v "true") (and (eq $v "auto") (.Capabilities.APIVersions.Has "security.openshift.io/v1")) -}}true{{- end -}}
{{- end }}
