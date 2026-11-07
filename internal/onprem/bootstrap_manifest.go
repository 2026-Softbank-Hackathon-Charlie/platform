package onprem

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// BootstrapConfig is server-controlled configuration, never an uploaded manifest.
// Credentials are emitted only to the authenticated installer, not to status views.
type BootstrapConfig struct {
	EnrollmentID, Origin, ConnectionToken, Hostname, TailnetAuthKey string
	AgentImage, TailscaleImage, AgentArch, AgentURL, AgentSHA256    string
}

var bootstrapPinnedImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./:_-]*@sha256:[a-f0-9]{64}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var credentialPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{32,512}$`)

// BootstrapManifest creates the local connector and delegated identities. The
// local administrator applies it; no administrator kubeconfig leaves the host.
func BootstrapManifest(c BootstrapConfig) ([]byte, error) {
	base, err := origin(c.Origin)
	if err != nil || !idPattern.MatchString(c.EnrollmentID) || c.Hostname != "hop-k3s-"+strings.TrimPrefix(c.EnrollmentID, "enr_") || !credentialPattern.MatchString(c.ConnectionToken) || !strings.HasPrefix(c.TailnetAuthKey, "tskey-auth-") || strings.ContainsAny(c.TailnetAuthKey, "\r\n\" ") || !bootstrapPinnedImagePattern.MatchString(c.AgentImage) || !bootstrapPinnedImagePattern.MatchString(c.TailscaleImage) || (c.AgentArch != "amd64" && c.AgentArch != "arm64") || c.AgentURL != base+"/v1/onprem/agent.bin?arch="+c.AgentArch || !digestPattern.MatchString(c.AgentSHA256) {
		return nil, errors.New("invalid onprem bootstrap configuration")
	}
	const ns = "charlie-system"
	name := "hop-" + strings.TrimPrefix(c.EnrollmentID, "enr_")
	agent, platform, argo := name+"-agent", name+"-platform", name+"-argo"
	labels := map[string]any{"hopcharlie.dev/enrollment": c.EnrollmentID, "app.kubernetes.io/name": name}
	obj := func(api, kind, n string, spec map[string]any) map[string]any {
		o := map[string]any{"apiVersion": api, "kind": kind, "metadata": map[string]any{"name": n, "labels": labels}}
		for k, v := range spec {
			o[k] = v
		}
		return o
	}
	namespaced := func(api, kind, n string, spec map[string]any) map[string]any {
		o := obj(api, kind, n, spec)
		o["metadata"].(map[string]any)["namespace"] = ns
		return o
	}
	rule := func(group string, resources, verbs []string) map[string]any {
		return map[string]any{"apiGroups": []string{group}, "resources": resources, "verbs": verbs}
	}
	role := func(n string, rules []any) map[string]any {
		return obj("rbac.authorization.k8s.io/v1", "ClusterRole", n, map[string]any{"rules": rules})
	}
	binding := func(n, sa string) map[string]any {
		return obj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", n, map[string]any{"roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": n}, "subjects": []any{map[string]any{"kind": "ServiceAccount", "name": sa, "namespace": ns}}})
	}
	items := []any{map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns}}}
	probeNamespace := "hop-check-" + strings.TrimPrefix(c.EnrollmentID, "enr_")
	items = append(items, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": probeNamespace, "labels": map[string]string{"hopcharlie.dev/onprem-enrollment": c.EnrollmentID, "pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/audit": "restricted", "pod-security.kubernetes.io/warn": "restricted"}}})
	for _, sa := range []string{agent, platform, argo} {
		items = append(items, namespaced("v1", "ServiceAccount", sa, map[string]any{"automountServiceAccountToken": false}))
	}
	items = append(items,
		namespaced("v1", "Secret", name+"-connection", map[string]any{"type": "Opaque", "stringData": map[string]string{"token": c.ConnectionToken}}),
		namespaced("v1", "Secret", name+"-tailscale-auth", map[string]any{"type": "Opaque", "stringData": map[string]string{"authkey": c.TailnetAuthKey}}),
		namespaced("v1", "Secret", name+"-tailscale-state", map[string]any{"type": "Opaque"}),
	)
	tokenRule := rule("", []string{"serviceaccounts/token"}, []string{"create"})
	tokenRule["resourceNames"] = []string{platform, argo}
	stateRule := rule("", []string{"secrets"}, []string{"get", "update", "patch"})
	stateRule["resourceNames"] = []string{name + "-tailscale-state"}
	items = append(items, namespaced("rbac.authorization.k8s.io/v1", "Role", agent, map[string]any{"rules": []any{tokenRule, stateRule}}), namespaced("rbac.authorization.k8s.io/v1", "RoleBinding", agent, map[string]any{"roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": agent}, "subjects": []any{map[string]any{"kind": "ServiceAccount", "name": agent, "namespace": ns}}}))
	namespaceRule := rule("", []string{"namespaces"}, []string{"get"})
	namespaceRule["resourceNames"] = []string{"kube-system"}
	observerRules := []any{namespaceRule, rule("", []string{"nodes", "pods", "services"}, []string{"get", "list"}), rule("postgresql.cnpg.io", []string{"clusters"}, []string{"get", "list"}), rule("barmancloud.cnpg.io", []string{"objectstores"}, []string{"list"}), rule("networking.k8s.io", []string{"networkpolicies"}, []string{"list"}), rule("rbac.authorization.k8s.io", []string{"clusterrolebindings"}, []string{"list"})}
	// Agent gets only the cluster's immutable identifier, never workload Secrets.
	items = append(items, role(agent, []any{namespaceRule}), binding(agent, agent), role(platform, observerRules), binding(platform, platform))
	argoRules := []any{rule("", []string{"namespaces"}, []string{"get", "list", "watch", "create"}), rule("", []string{"pods", "pods/log", "services", "persistentvolumeclaims", "resourcequotas", "limitranges", "configmaps"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}), rule("apps", []string{"deployments", "replicasets", "statefulsets"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}), rule("batch", []string{"jobs", "cronjobs"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}), rule("networking.k8s.io", []string{"networkpolicies", "ingresses"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}), rule("bitnami.com", []string{"sealedsecrets"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}), rule("postgresql.cnpg.io", []string{"clusters"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"})}
	items = append(items, role(argo, argoRules), binding(argo, argo))
	// The server's HTTP probe may proxy only this enrollment's hello Pod.
	// Proxy GET is not covered by write admission, so it must not be global.
	proxyRule := rule("", []string{"pods/proxy"}, []string{"get"})
	proxyRule["resourceNames"] = []string{"hello", "hello:8080"}
	proxyRole := namespaced("rbac.authorization.k8s.io/v1", "Role", name+"-hello-proxy", map[string]any{"rules": []any{proxyRule}})
	proxyRole["metadata"].(map[string]any)["namespace"] = probeNamespace
	proxyBinding := namespaced("rbac.authorization.k8s.io/v1", "RoleBinding", name+"-hello-proxy", map[string]any{"roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name + "-hello-proxy"}, "subjects": []any{map[string]any{"kind": "ServiceAccount", "name": argo, "namespace": ns}}})
	proxyBinding["metadata"].(map[string]any)["namespace"] = probeNamespace
	items = append(items, proxyRole, proxyBinding)
	protected := `['kube-system','kube-public','kube-node-lease','default','charlie-system','argocd','hopcharlie-db-network','observability','minio']`
	user := "system:serviceaccount:" + ns + ":" + argo
	ownerLabel := "hopcharlie.dev/onprem-enrollment"
	// Admission is scoped to the delegated Argo identity. It cannot edit this
	// policy or RBAC, and cannot access the connector's credential namespace.
	policy := obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicy", argo, map[string]any{"spec": map[string]any{
		"failurePolicy":    "Fail",
		"matchConstraints": map[string]any{"resourceRules": []any{map[string]any{"apiGroups": []string{"*"}, "apiVersions": []string{"*"}, "operations": []string{"CREATE", "UPDATE", "DELETE", "CONNECT"}, "resources": []string{"*", "*/*"}}}},
		"matchConditions":  []any{map[string]any{"name": "delegated-identity", "expression": "request.userInfo.username == '" + user + "'"}},
		"validations": []any{
			map[string]any{"expression": "!(request.namespace in " + protected + ") && !request.namespace.startsWith('kube-') && (request.resource.resource != 'namespaces' || (object != null && !(object.metadata.name in " + protected + ") && !object.metadata.name.startsWith('kube-')))", "message": "the delegated installer identity cannot change protected namespaces"},
			map[string]any{"expression": "request.namespace == '' || (namespaceObject != null && has(namespaceObject.metadata.labels) && 'pod-security.kubernetes.io/enforce' in namespaceObject.metadata.labels && namespaceObject.metadata.labels['pod-security.kubernetes.io/enforce'] == 'restricted' && '" + ownerLabel + "' in namespaceObject.metadata.labels && namespaceObject.metadata.labels['" + ownerLabel + "'] == '" + c.EnrollmentID + "')", "message": "delegated changes require this enrollment's restricted namespace"},
			map[string]any{"expression": "request.resource.resource != 'namespaces' || (object != null && has(object.metadata.labels) && 'pod-security.kubernetes.io/enforce' in object.metadata.labels && object.metadata.labels['pod-security.kubernetes.io/enforce'] == 'restricted' && '" + ownerLabel + "' in object.metadata.labels && object.metadata.labels['" + ownerLabel + "'] == '" + c.EnrollmentID + "')", "message": "new application namespaces must belong to this enrollment and enforce restricted Pod Security"},
		},
	}})
	items = append(items, policy, obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBinding", argo, map[string]any{"spec": map[string]any{"policyName": argo, "validationActions": []string{"Deny"}}}))
	// Owning a namespace does not delegate its other ServiceAccounts. In
	// particular, a Pod must not mint or mount a stronger account's credentials.
	workloadPolicyName := argo + "-workloads"
	workloadPolicy := obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicy", workloadPolicyName, map[string]any{"spec": map[string]any{
		"failurePolicy":    "Fail",
		"matchConstraints": map[string]any{"resourceRules": []any{map[string]any{"apiGroups": []string{"", "apps", "batch"}, "apiVersions": []string{"*"}, "operations": []string{"CREATE", "UPDATE"}, "resources": []string{"pods", "deployments", "replicasets", "statefulsets", "jobs", "cronjobs"}}}},
		"matchConditions":  []any{map[string]any{"name": "delegated-identity", "expression": "request.userInfo.username == '" + user + "'"}},
		"variables":        []any{map[string]any{"name": "pod", "expression": "request.resource.resource == 'pods' ? object.spec : (request.resource.resource == 'cronjobs' ? object.spec.jobTemplate.spec.template.spec : object.spec.template.spec)"}},
		"validations": []any{
			map[string]any{"expression": "!has(variables.pod.serviceAccountName) || variables.pod.serviceAccountName == '' || variables.pod.serviceAccountName == 'default'", "message": "delegated workloads cannot select another ServiceAccount"},
			map[string]any{"expression": "has(variables.pod.automountServiceAccountToken) && variables.pod.automountServiceAccountToken == false", "message": "delegated workloads must disable automatic ServiceAccount token mounts"},
			map[string]any{"expression": "!has(variables.pod.volumes) || variables.pod.volumes.all(v, !has(v.projected) || !has(v.projected.sources) || v.projected.sources.all(s, !has(s.serviceAccountToken)))", "message": "delegated workloads cannot project ServiceAccount tokens"},
		},
	}})
	items = append(items, workloadPolicy, obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBinding", workloadPolicyName, map[string]any{"spec": map[string]any{"policyName": workloadPolicyName, "validationActions": []string{"Deny"}}}))
	serve, _ := json.Marshal(map[string]any{"TCP": map[string]any{"6443": map[string]any{"TCPForward": "kubernetes.default.svc:443"}}})
	items = append(items, namespaced("v1", "ConfigMap", name+"-serve", map[string]any{"data": map[string]string{"serve.json": string(serve)}}))
	env := func(n, v string) map[string]any { return map[string]any{"name": n, "value": v} }
	mount := func(n, p string) map[string]any { return map[string]any{"name": n, "mountPath": p, "readOnly": true} }
	security := map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}, "readOnlyRootFilesystem": true, "runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001}
	resources := map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]string{"cpu": "200m", "memory": "128Mi"}}
	download := map[string]any{"name": "download-agent", "image": c.AgentImage, "command": []string{"/bin/sh", "-ec", `wget -q -O /agent/bin "$AGENT_URL"; printf '%s  %s\n' "$AGENT_SHA256" /agent/bin | sha256sum -c -; chmod 0555 /agent/bin`}, "env": []any{env("AGENT_URL", c.AgentURL), env("AGENT_SHA256", c.AgentSHA256)}, "volumeMounts": []any{map[string]any{"name": "binary", "mountPath": "/agent"}}, "securityContext": security, "resources": resources}
	agentContainer := map[string]any{"name": "connector", "image": c.AgentImage, "command": []string{"/agent/bin"}, "env": []any{env("SERVER", base), env("ENROLLMENT_ID", c.EnrollmentID), env("CONNECTION_TOKEN_FILE", "/connection/token"), env("PLATFORM_SERVICE_ACCOUNT", platform), env("ARGO_SERVICE_ACCOUNT", argo)}, "volumeMounts": []any{mount("binary", "/agent"), mount("connection", "/connection"), mount("api-access", "/var/run/secrets/kubernetes.io/serviceaccount")}, "securityContext": security, "resources": resources}
	tailscale := map[string]any{"name": "tailscale", "image": c.TailscaleImage, "env": []any{env("TS_USERSPACE", "true"), env("TS_AUTH_ONCE", "true"), env("TS_HOSTNAME", c.Hostname), env("TS_KUBE_SECRET", name+"-tailscale-state"), env("TS_SERVE_CONFIG", "/serve/serve.json"), map[string]any{"name": "TS_AUTHKEY", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": name + "-tailscale-auth", "key": "authkey"}}}}, "volumeMounts": []any{mount("serve", "/serve"), mount("api-access", "/var/run/secrets/kubernetes.io/serviceaccount"), map[string]any{"name": "tailscale-run", "mountPath": "/tmp"}}, "securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}}, "resources": resources}
	apiAccess := map[string]any{
		"name": "api-access",
		"projected": map[string]any{
			"sources": []any{
				map[string]any{"serviceAccountToken": map[string]any{"path": "token", "expirationSeconds": 3600}},
				map[string]any{"configMap": map[string]any{"name": "kube-root-ca.crt", "items": []any{map[string]any{"key": "ca.crt", "path": "ca.crt"}}}},
				map[string]any{"downwardAPI": map[string]any{"items": []any{map[string]any{"path": "namespace", "fieldRef": map[string]any{"fieldPath": "metadata.namespace"}}}}},
			},
		},
	}
	podSpec := map[string]any{"serviceAccountName": agent, "automountServiceAccountToken": false, "enableServiceLinks": false, "nodeSelector": map[string]string{"kubernetes.io/arch": c.AgentArch}, "securityContext": map[string]any{"fsGroup": 10001, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}, "initContainers": []any{download}, "containers": []any{agentContainer, tailscale}, "volumes": []any{map[string]any{"name": "binary", "emptyDir": map[string]any{}}, map[string]any{"name": "tailscale-run", "emptyDir": map[string]any{}}, map[string]any{"name": "connection", "secret": map[string]any{"secretName": name + "-connection", "defaultMode": int32(0440)}}, map[string]any{"name": "serve", "configMap": map[string]any{"name": name + "-serve"}}, apiAccess}}
	items = append(items, namespaced("apps/v1", "Deployment", name, map[string]any{"spec": map[string]any{"replicas": 1, "strategy": map[string]any{"type": "Recreate"}, "selector": map[string]any{"matchLabels": labels}, "template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": podSpec}}}))
	return json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
}
