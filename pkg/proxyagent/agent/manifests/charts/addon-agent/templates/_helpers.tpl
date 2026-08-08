{{/*
Return true when the rendered service-proxy configuration may authenticate an
identity that must be forwarded with Kubernetes impersonation.
*/}}
{{- define "cluster-proxy-agent.requiresImpersonation" -}}
{{- $requiresImpersonation := or (has .Values.enableImpersonation (list "1" "t" "T" "TRUE" "true" "True")) (not (empty .Values.oidcIssuerURL)) -}}
{{- $additionalHubAuthConfigured := false -}}
{{- $additionalOIDCIssuerConfigured := false -}}
{{- $duplicateAdditionalOIDCIssuer := false -}}
{{- range $arg := .Values.additionalServiceProxyArgs -}}
  {{- if or (eq $arg "--enable-impersonation") (hasPrefix "--enable-impersonation=" $arg) -}}
    {{- $additionalHubAuthConfigured = true -}}
  {{- end -}}
  {{- if or (eq $arg "--oidc-issuer-url") (hasPrefix "--oidc-issuer-url=" $arg) -}}
    {{- if $additionalOIDCIssuerConfigured -}}
      {{- $duplicateAdditionalOIDCIssuer = true -}}
    {{- else -}}
      {{- $additionalOIDCIssuerConfigured = true -}}
    {{- end -}}
  {{- end -}}
  {{- $enablesOIDCAuth := or (eq $arg "--oidc-issuer-url") (and (hasPrefix "--oidc-issuer-url=" $arg) (ne $arg "--oidc-issuer-url=")) -}}
  {{- if $enablesOIDCAuth -}}
    {{- $requiresImpersonation = true -}}
  {{- end -}}
{{- end -}}
{{- if and .Values.enableServiceProxy $additionalHubAuthConfigured -}}
  {{- fail "additionalServiceProxyArgs must not set --enable-impersonation; use enableImpersonation instead" -}}
{{- end -}}
{{- if and .Values.enableServiceProxy $duplicateAdditionalOIDCIssuer -}}
  {{- fail "additionalServiceProxyArgs must set --oidc-issuer-url at most once" -}}
{{- end -}}
{{- if and .Values.enableServiceProxy (not (empty .Values.oidcIssuerURL)) $additionalOIDCIssuerConfigured -}}
  {{- fail "configure the OIDC issuer with either oidcIssuerURL or --oidc-issuer-url in additionalServiceProxyArgs, not both" -}}
{{- end -}}
{{- $requiresImpersonation -}}
{{- end -}}
