{{/*
Chart identity for the helm.sh/chart label (truncated per the k8s label
value rules).
*/}}
{{- define "stonks.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | trunc 63 | trimSuffix "-" -}}
{{- end -}}


{{/*
Full label set for every object this chart creates.
Usage: {{ include "stonks.labels" (dict "root" . "component" "backend") }}
*/}}
{{- define "stonks.labels" -}}
app.kubernetes.io/name: stonks
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
helm.sh/chart: {{ include "stonks.chart" .root }}
{{- end -}}


{{/*
STABLE subset of `stonks.labels` — used for pod-template selectors.
A selector can never be changed on a live Deployment/StatefulSet, so
this set is frozen for the life of a release.
Usage: {{ include "stonks.selectorLabels" (dict "root" . "component" "backend") }}
*/}}
{{- define "stonks.selectorLabels" -}}
app.kubernetes.io/name: stonks
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}
