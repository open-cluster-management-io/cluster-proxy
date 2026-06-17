{{/*
Image tag shared across cluster-proxy images, defaulting to the chart version.
*/}}
{{- define "cluster-proxy-common.imageTag" -}}
{{- .Values.tag | default (print "v" .Chart.Version) -}}
{{- end -}}

{{/*
Return the chart's primary cluster-proxy image.
*/}}
{{- define "cluster-proxy-common.clusterProxyImage" -}}
{{- printf "%s/%s:%s" .Values.registry .Values.image (include "cluster-proxy-common.imageTag" .) -}}
{{- end -}}

{{/*
Return the configured proxy-server image.
*/}}
{{- define "cluster-proxy-common.proxyServerImage" -}}
{{- printf "%s:%s" .Values.proxyServerImage (include "cluster-proxy-common.imageTag" .) -}}
{{- end -}}

{{/*
Return the configured proxy-agent image.
*/}}
{{- define "cluster-proxy-common.proxyAgentRepositoryImage" -}}
{{- printf "%s:%s" .Values.proxyAgentImage (include "cluster-proxy-common.imageTag" .) -}}
{{- end -}}

{{/*
Return proxyAgentImage when set, otherwise the chart's primary image.
*/}}
{{- define "cluster-proxy-common.proxyAgentImageOrDefault" -}}
{{- .Values.proxyAgentImage | default (include "cluster-proxy-common.clusterProxyImage" .) -}}
{{- end -}}
