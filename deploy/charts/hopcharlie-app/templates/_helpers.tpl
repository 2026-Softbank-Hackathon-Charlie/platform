{{/*
Labels of a service's objects. Call with (dict "root" $ "svc" $svc).
*/}}
{{- define "hopcharlie-app.selectorLabels" -}}
hopcharlie.dev/app: {{ .root.Values.values.app }}
hopcharlie.dev/service: {{ .svc.name }}
{{- end -}}

{{- define "hopcharlie-app.labels" -}}
{{ include "hopcharlie-app.selectorLabels" . }}
app.kubernetes.io/name: {{ .svc.name }}
app.kubernetes.io/instance: {{ .root.Release.Namespace }}
app.kubernetes.io/part-of: {{ .root.Values.values.app }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
{{- end -}}

{{/*
Labels of the namespace's own objects (quota, limits, network policy).
*/}}
{{- define "hopcharlie-app.namespaceLabels" -}}
hopcharlie.dev/app: {{ .Values.values.app }}
app.kubernetes.io/part-of: {{ .Values.values.app }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
The resource of a type, or an empty dict. Fails when there are two: the
variables of a second one would have no names (platform ADR 0011).
*/}}
{{- define "hopcharlie-app.resource" -}}
{{- $found := dict -}}
{{- range .root.Values.values.resources -}}
{{- if eq .type $.type -}}
{{- if $found.name -}}
{{- fail (printf "values.resources: only one %s resource is allowed (%s and %s)" $.type $found.name .name) -}}
{{- end -}}
{{- $_ := set $found "name" .name -}}
{{- end -}}
{{- end -}}
{{- toJson $found -}}
{{- end -}}

{{/*
The variables the platform sets in every container (pkg/appspec Reserved).
A service without a port gets no PORT. They come before the service's own
env, so its values can refer to them as $(PGHOST).
*/}}
{{- define "hopcharlie-app.platformEnv" -}}
{{- $root := .root -}}
{{- $app := $root.Values.values.app -}}
{{- with .svc.port }}
- name: PORT
  value: {{ . | quote }}
{{- end }}
{{- $pg := include "hopcharlie-app.resource" (dict "root" $root "type" "postgres") | fromJson }}
{{- with $pg.name }}
{{- $secret := printf "%s-%s-app" $app . }}
{{- /* Variable and key of CNPG's app secret: the connection string and its parts. */}}
{{- range list "DATABASE_URL uri" "PGHOST host" "PGPORT port" "PGDATABASE dbname" "PGUSER username" "PGPASSWORD password" "JDBC_DATABASE_URL jdbc-uri" }}
- name: {{ first (splitList " " .) }}
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: {{ last (splitList " " .) }}
{{- end }}
{{- end }}
{{- $s3res := include "hopcharlie-app.resource" (dict "root" $root "type" "s3") | fromJson }}
{{- with $s3res.name }}
{{- $s3 := $root.Values.profile.s3 }}
{{- if or (not $s3.endpoint) (not $s3.credentialsSecret) }}
{{- fail "profile.s3.endpoint and profile.s3.credentialsSecret are required for an s3 resource" }}
{{- end }}
- name: S3_ENDPOINT
  value: {{ $s3.endpoint | quote }}
# One bucket per instance, named after it (platform ADR 0016).
- name: S3_BUCKET
  value: {{ printf "%s%s" ($s3.bucketPrefix | default "") $root.Release.Namespace | quote }}
- name: S3_REGION
  value: {{ $s3.region | default "" | quote }}
- name: S3_PATH_STYLE
  value: {{ $s3.pathStyle | default false | toString | quote }}
- name: S3_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $s3.credentialsSecret }}
      key: {{ $s3.accessKeyKey | default "accessKey" }}
- name: S3_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $s3.credentialsSecret }}
      key: {{ $s3.secretKeyKey | default "secretKey" }}
{{- end }}
{{- end -}}

{{/*
Requests and limits of a service's container: profile.resources, with the
service's memory as the memory request and limit and its cpu as the CPU
request.
*/}}
{{- define "hopcharlie-app.resources" -}}
{{- $res := deepCopy (.root.Values.profile.resources | default dict) -}}
{{- $requests := $res.requests | default dict -}}
{{- $limits := $res.limits | default dict -}}
{{- with .svc.memory -}}
{{- $_ := set $requests "memory" . -}}
{{- $_ := set $limits "memory" . -}}
{{- end -}}
{{- with .svc.cpu -}}
{{- $_ := set $requests "cpu" . -}}
{{- end -}}
{{- if $requests }}{{ $_ := set $res "requests" $requests }}{{ end -}}
{{- if $limits }}{{ $_ := set $res "limits" $limits }}{{ end -}}
{{- with $res }}{{ toYaml . }}{{ end -}}
{{- end -}}

{{/*
A SealedSecret in the release namespace. strict scope: the ciphertexts open
only as this name in this namespace. Call with (dict "root" $ "name" <name>
"data" <key: ciphertext>).
*/}}
{{- define "hopcharlie-app.sealedSecret" -}}
apiVersion: bitnami.com/v1alpha1
kind: SealedSecret
metadata:
  name: {{ .name }}
  namespace: {{ .root.Release.Namespace }}
  labels:
    {{- include "hopcharlie-app.namespaceLabels" .root | nindent 4 }}
  annotations:
    # With the namespace, before the Deployments: Argo CD waits until the
    # controller has unsealed it, so new pods read the new values.
    argocd.argoproj.io/sync-wave: "-1"
spec:
  encryptedData:
    {{- toYaml .data | nindent 4 }}
  template:
    metadata:
      name: {{ .name }}
      namespace: {{ .root.Release.Namespace }}
    type: Opaque
{{- end -}}

{{/*
Fails on what the schema cannot say: unique service names, at most one
public service, and no env name that is also a sealed app secret.
*/}}
{{- define "hopcharlie-app.check" -}}
{{- $names := dict -}}
{{- $public := list -}}
{{- range .Values.values.services -}}
{{- if hasKey $names .name -}}
{{- fail (printf "values.services: two services are named %s" .name) -}}
{{- end -}}
{{- $_ := set $names .name true -}}
{{- if .public -}}
{{- $public = append $public .name -}}
{{- end -}}
{{- $svc := .name -}}
{{- range $k, $_ := .env -}}
{{- if hasKey ($.Values.values.sealedSecrets.app | default dict) $k -}}
{{- fail (printf "values.services[%s].env.%s is also in values.sealedSecrets.app; the secret must win, so env must not set it" $svc $k) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if gt (len $public) 1 -}}
{{- fail (printf "values.services: only one service may be public (%s)" (join ", " $public)) -}}
{{- end -}}
{{- end -}}
