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
"data" <key: ciphertext>), and "type" for a Secret type other than Opaque.
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
    type: {{ .type | default "Opaque" }}
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
{{- $topo := include "hopcharlie-app.dbTopology" . | fromJson -}}
{{- with $topo.cluster -}}
{{- $services := list $topo.ingressService -}}
{{- $hosts := list $topo.ingressHost -}}
{{- range $topo.peers -}}
{{- $services = append $services .egressService -}}
{{- $hosts = concat $hosts (list .egressHost .peerHost) -}}
{{- end -}}
{{- range $services -}}
{{- if gt (len .) 63 -}}
{{- fail (printf "values.dbTopology: Service name %s is longer than 63 characters" .) -}}
{{- end -}}
{{- if hasKey $names (trimPrefix (printf "%s-" $.Values.values.app) .) -}}
{{- fail (printf "values.dbTopology: Service %s is also an app service's Service" .) -}}
{{- end -}}
{{- end -}}
{{- range $hosts -}}
{{- if gt (len .) 63 -}}
{{- fail (printf "values.dbTopology: Tailscale hostname %s is longer than 63 characters" .) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The names of the postgres resource's half of a CNPG distributed topology
(values.dbTopology, infra ADR 0063) as JSON, or {} without dbTopology. The
gitops values, these names and the Secret names are the contract with the
platform, which builds the same names for the migration engine:

  cluster          <app>-<pg>
  self, other      <cluster>-<env>, <cluster>-<other env>: replica.self and
                   the externalClusters
  ingressService   <cluster>-ts, the primary exposed on the tailnet as
                   hop-db-<env>-<instance>
  egressService    <cluster>-to-<other env>, the peer's database through the
                   tailnet host hop-db-<env>-<instance>-to-<other env>

Without members, other is the environment of the file that is not env. The platform writes peer
as the environment born a replica, the same value in both files; a peer that
is the other environment seen from this file is accepted too. peerPresent
false (infra ADR 0074) means the peer environment does not exist yet: only
the primary's file may say so, and dbnetwork.yaml then draws no tailnet
objects. With members, primary and peer are still the legacy pair and all
members get externalClusters and egress paths. Every nonprimary follows
primary; primary keeps peer as its default demotion source. Fails on values
the platform must not write.
*/}}
{{- define "hopcharlie-app.dbTopology" -}}
{{- $t := .Values.values.dbTopology -}}
{{- $out := dict -}}
{{- if $t -}}
{{- $sealed := .Values.values.sealedSecrets | default dict -}}
{{- $pg := include "hopcharlie-app.resource" (dict "root" . "type" "postgres") | fromJson -}}
{{- if not (and $pg.name $sealed.pgApp $sealed.pgCA) -}}
{{- fail "values.dbTopology needs a postgres resource, values.sealedSecrets.pgApp and values.sealedSecrets.pgCA (infra ADR 0063)" -}}
{{- end -}}
{{- $env := .Values.env -}}
{{- $members := list $t.primary $t.peer -}}
{{- $explicitMembers := hasKey $t "members" -}}
{{- $other := ternary $t.peer $t.primary (eq $env $t.primary) -}}
{{- if $explicitMembers -}}
{{- $members = $t.members -}}
{{- if or (lt (len $members) 2) (gt (len $members) 3) (ne (len $members) (len (uniq $members))) (eq $t.primary $t.peer) (not (has $t.primary $members)) (not (has $t.peer $members)) (not (has $env $members)) -}}
{{- fail "values.dbTopology.members must contain 2..3 unique environments including primary, distinct peer and env" -}}
{{- end -}}
{{- else if or (not $env) (eq $other $env) (not (has $t.peer (list $env $other))) -}}
{{- fail (printf "values.dbTopology: env %q must be one of primary %q and peer %q, and the other must differ (infra ADR 0063)" $env $t.primary $t.peer) -}}
{{- end -}}
{{- $peerPresent := not (and (hasKey $t "peerPresent") (not $t.peerPresent)) -}}
{{- if and $explicitMembers (not $peerPresent) -}}
{{- fail "values.dbTopology.members cannot be combined with peerPresent false" -}}
{{- end -}}
{{- if and (not $peerPresent) (ne $env $t.primary) -}}
{{- fail (printf "values.dbTopology.peerPresent false is only for the primary's file (env %q, primary %q; infra ADR 0074)" $env $t.primary) -}}
{{- end -}}
{{- $cluster := printf "%s-%s" .Values.values.app $pg.name -}}
{{- $instance := .Release.Namespace -}}
{{- $peers := list -}}
{{- range $members -}}
{{- if ne . $env -}}
{{- $peers = append $peers (dict "env" . "name" (printf "%s-%s" $cluster .) "egressService" (printf "%s-to-%s" $cluster .) "egressHost" (printf "hop-db-%s-%s-to-%s" $env $instance .) "peerHost" (printf "hop-db-%s-%s" . $instance)) -}}
{{- end -}}
{{- end -}}
{{- $_ := set $out "cluster" $cluster -}}
{{- $_ := set $out "members" $members -}}
{{- $_ := set $out "explicitMembers" $explicitMembers -}}
{{- $_ := set $out "peers" $peers -}}
{{- $_ := set $out "peerPresent" $peerPresent -}}
{{- $_ := set $out "env" $env -}}
{{- $_ := set $out "other" $other -}}
{{- $_ := set $out "isPrimary" (eq $env $t.primary) -}}
{{- $_ := set $out "self" (printf "%s-%s" $cluster $env) -}}
{{- $_ := set $out "primary" (printf "%s-%s" $cluster $t.primary) -}}
{{- $_ := set $out "source" (printf "%s-%s" $cluster $other) -}}
{{- $_ := set $out "ingressService" (printf "%s-ts" $cluster) -}}
{{- $_ := set $out "egressService" (printf "%s-to-%s" $cluster $other) -}}
{{- $_ := set $out "ingressHost" (printf "hop-db-%s-%s" $env $instance) -}}
{{- $_ := set $out "egressHost" (printf "hop-db-%s-%s-to-%s" $env $instance $other) -}}
{{- $_ := set $out "peerHost" (printf "hop-db-%s-%s" $other $instance) -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{/* Namespace-independent OTel wiring; no credential in app values. */}}
{{- define "hopcharlie-app.otelEnv" -}}
{{- $o := .root.Values.profile.observability -}}
{{- if $o.enabled -}}
{{- range $key, $_ := .svc.env -}}
{{- if hasPrefix "OTEL_" $key -}}
{{- fail "OTEL_* is managed by the enabled environment observability profile" -}}
{{- end -}}
{{- end -}}
{{- range $key, $value := .root.Values.values.sealedSecrets.app -}}
{{- if hasPrefix "OTEL_" $key -}}
{{- fail "OTEL_* sealed variables conflict with the environment observability profile" -}}
{{- end -}}
{{- end -}}
- {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: {{ $o.endpoint | quote }}}
- {name: OTEL_EXPORTER_OTLP_PROTOCOL, value: "http/protobuf"}
- {name: OTEL_EXPORTER_OTLP_CERTIFICATE, value: /etc/otel/ca/ca.crt}
- {name: OTEL_EXPORTER_OTLP_INSECURE, value: "false"}
- {name: OTEL_TRACES_EXPORTER, value: otlp}
- {name: OTEL_METRICS_EXPORTER, value: none}
- {name: OTEL_LOGS_EXPORTER, value: none}
- {name: OTEL_SERVICE_NAME, value: {{ printf "%s-%s" .root.Values.values.app .svc.name | quote }}}
- name: OTEL_K8S_POD_UID
  valueFrom: {fieldRef: {fieldPath: metadata.uid}}
- name: OTEL_RESOURCE_ATTRIBUTES
  value: {{ printf "service.version=%s,k8s.cluster.name=%s,k8s.namespace.name=%s,k8s.pod.uid=$(OTEL_K8S_POD_UID),cloud.provider=%s,cloud.region=%s,deployment.environment.name=%s" (.root.Values.commit | default "unknown") $o.clusterName .root.Release.Namespace $o.cloudProvider $o.region (.root.Values.env | default "unknown") | quote }}
{{- end -}}
{{- end -}}
