{{/*
Application environment for the backend and worker pods. Two sources —
this chart is PUBLIC and never carries secret VALUES:

- The pre-existing Secret named by secrets.existingSecret carries only
  the genuinely secret values: KEK, DATABASE_URL, POSTGRES_PASSWORD, and
  the optional OIDC_CLIENT_SECRET / NR_LICENSE_KEY / SMTP_PASS /
  LLM_API_KEY. Required secret refs have NO `optional:` flag (a missing
  key fails pod startup with a clear CreateContainerConfigError);
  optional refs get `optional: true` and render as empty strings when
  absent.
- values.yaml carries everything non-secret (OIDC_ISSUER, SMTP_HOST,
  ...). Required value keys fail the RENDER when empty (`required`);
  optional ones render as empty strings, which the app treats as "feature
  disabled" (backend/internal/config/config.go uses os.Getenv — a
  missing env var IS the empty string).

Usage: {{ include "stonks.appEnv" . }}
*/}}
{{- define "stonks.appEnv" -}}
# --- required: secret (fail-fast pod startup) ---
- name: KEK
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: KEK
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: DATABASE_URL
# --- required: non-secret values (fail-fast render) ---
- name: OIDC_ISSUER
  value: {{ required "oidc.issuer is required (the OIDC issuer URL)" .Values.oidc.issuer | quote }}
- name: OIDC_CLIENT_ID
  value: {{ required "oidc.clientId is required (the first-party SPA client ID)" .Values.oidc.clientId | quote }}
- name: OIDC_AUDIENCE
  value: {{ required "oidc.audience is required (the resource server identifier)" .Values.oidc.audience | quote }}
# --- optional: secret (absent key = empty = feature disabled) ---
- name: OIDC_CLIENT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: OIDC_CLIENT_SECRET
      optional: true
- name: NR_LICENSE_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: NR_LICENSE_KEY
      optional: true
- name: SMTP_PASS
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: SMTP_PASS
      optional: true
- name: LLM_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: LLM_API_KEY
      optional: true
# --- optional: non-secret values (empty = feature disabled) ---
- name: OIDC_DISCOVERY_URL
  value: {{ .Values.oidc.discoveryUrl | quote }}
- name: NR_APPLICATION_ID
  value: {{ .Values.newrelic.applicationId | quote }}
- name: SMTP_HOST
  value: {{ .Values.smtp.host | quote }}
- name: SMTP_PORT
  value: {{ .Values.smtp.port | quote }}
- name: SMTP_USER
  value: {{ .Values.smtp.user | quote }}
- name: SMTP_FROM
  value: {{ .Values.smtp.from | quote }}
- name: SMTP_TLS_POLICY
  value: {{ .Values.smtp.tlsPolicy | quote }}
- name: LLM_BASE_URL
  value: {{ .Values.llm.baseUrl | quote }}
- name: LLM_MODEL
  value: {{ .Values.llm.model | quote }}
{{- end -}}
{{/*
Application environment for the frontend Deployment: the container
entrypoint renders the runtime CSP from OIDC_ISSUER (required value) and
gates the New Relic browser agent in that CSP on the two NR keys (secret
+ value); the SPA itself reads OIDC settings at runtime from the
backend's GET /api/v1/public-config.
*/}}
{{- define "stonks.frontendEnv" -}}
- name: OIDC_ISSUER
  value: {{ required "oidc.issuer is required (the OIDC issuer URL)" .Values.oidc.issuer | quote }}
- name: NR_LICENSE_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: NR_LICENSE_KEY
      optional: true
- name: NR_APPLICATION_ID
  value: {{ .Values.newrelic.applicationId | quote }}
{{- end -}}
