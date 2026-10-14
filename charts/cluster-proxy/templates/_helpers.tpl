{{- define "cluster-proxy.proxyAgentArgs" -}}
{{- $proxyAgent := .Values.proxyAgent | default dict -}}
{{- $additionalArgs := $proxyAgent.additionalArgs | default list -}}
{{- $given := list -}}
{{- range $additionalArgs -}}
  {{- $given = append $given (first (splitList "=" .)) -}}
{{- end -}}
{{- $args := list -}}
{{- if and (gt (int .Values.replicas) 1) (not (has "--sync-forever" $given)) -}}
  {{- $args = append $args "--sync-forever" -}}
{{- end -}}
{{- if not (has "--keepalive-time" $given) -}}
  {{- $args = append $args "--keepalive-time=30s" -}}
{{- end -}}
{{- toYaml (concat $args $additionalArgs) -}}
{{- end -}}
