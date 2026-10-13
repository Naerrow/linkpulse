{{/* 리소스 이름. 릴리스 이름을 그대로 쓴다 — 사이트의 TargetGroupBinding이 Service 이름(linkpulse)을 가리킨다. */}}
{{- define "linkpulse.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "linkpulse.selectorLabels" -}}
app.kubernetes.io/name: linkpulse
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "linkpulse.labels" -}}
{{ include "linkpulse.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/*
이미지 참조. Image Updater는 tag 자리에 "main@sha256:..."을 쓴다(그대로 repository:tag로 붙인다).
tag가 다이제스트만("sha256:...")이면 repository@digest로 쓴다.
*/}}
{{- define "linkpulse.image" -}}
{{- $tag := .Values.image.tag | toString -}}
{{- if hasPrefix "sha256:" $tag -}}
{{ .Values.image.repository }}@{{ $tag }}
{{- else -}}
{{ .Values.image.repository }}:{{ $tag }}
{{- end -}}
{{- end -}}
