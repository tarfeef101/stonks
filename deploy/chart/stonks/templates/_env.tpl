{{/*
Application environment for the backend and worker pods. Every value
comes from the pre-existing Secret named by secrets.existingSecret —
this chart carries no secret values (public repo).

Required keys have NO `optional:` flag: if the key (or the Secret) is
missing, the pod fails at startup with a clear CreateContainerConfigError
instead of running misconfigured. Optional keys get `optional: true` and
render as empty strings when absent, which the app treats as "feature
disabled" (see backend/internal/config/config.go).

Usage: {{ include "stonks.appEnv" . }}
*/}}
{{- define "stonks.appEnv" -}}
# --- required (fail-fast on missing) ---
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
- name: OIDC_ISSUER
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: OIDC_ISSUER
- name: OIDC_CLIENT_ID
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: OIDC_CLIENT_ID
# --- optional (empty = feature disabled) ---
- name: OIDC_AUDIENCE
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: OIDC_AUDIENCE
      optional: true
- name: OIDC_CLIENT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: OIDC_CLIENT_SECRET
      optional: true
- name: OIDC_DISCOVERY_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: OIDC_DISCOVERY_URL
      optional: true
- name: NR_LICENSE_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: NR_LICENSE_KEY
      optional: true
- name: SMTP_HOST
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: SMTP_HOST
      optional: true
- name: SMTP_PORT
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: SMTP_PORT
      optional: true
- name: SMTP_USER
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: SMTP_USER
      optional: true
- name: SMTP_PASS
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: SMTP_PASS
      optional: true
- name: SMTP_FROM
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: SMTP_FROM
      optional: true
- name: SMTP_TLS_POLICY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: SMTP_TLS_POLICY
      optional: true
- name: LLM_BASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: LLM_BASE_URL
      optional: true
- name: LLM_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: LLM_API_KEY
      optional: true
- name: LLM_MODEL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.existingSecret }}
      key: LLM_MODEL
      optional: true
{{- end -}}
