package charts

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

const topologyValues = `env: %s
values:
  app: moimroom
  services:
    - {name: web, image: "r/a/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", port: 8080}
  resources: [{name: db, type: postgres}]
  dbTopology: {primary: gke, peer: eks%s}
  sealedSecrets:
    pgApp: {username: AgA1, password: AgA2, dbname: AgA3, host: AgA4, port: AgA5, uri: AgA6, jdbc-uri: AgA7}
    pgCA: {ca.crt: AgA8, ca.key: AgA9}
profile:
  networkPolicy: {enabled: true}
  dbNetwork: {enabled: true, tailnetDomain: tail567fe8.ts.net, ingressTag: "tag:hop-db-test", egressTag: "tag:hop-db-client-test", ingressProxyClass: app-ingress, egressProxyClass: app-egress, proxyNamespace: hopcharlie-db-network}
`

// Every environment must reach all other members while only the initial
// primary bootstraps with initdb. This catches the old two-member shortcut.
func TestThreeMemberTopology(t *testing.T) {
	for _, env := range []string{"gke", "eks", "onprem"} {
		t.Run(env, func(t *testing.T) {
			docs := mustRender(t, appChart, "moimroom", writeValues(t, fmt.Sprintf(topologyValues, env, ", members: [gke, eks, onprem]")))
			var cluster map[string]any
			if err := yaml.UnmarshalStrict(docs["Cluster/moimroom-db"], &cluster); err != nil {
				t.Fatal(err)
			}
			labels := cluster["metadata"].(map[string]any)["labels"].(map[string]any)
			if labels["hopcharlie.dev/db-topology-members"] != "3" {
				t.Fatalf("missing three-member marker: %v", labels)
			}
			spec := cluster["spec"].(map[string]any)
			replica := spec["replica"].(map[string]any)
			source := "gke"
			if env == "gke" {
				source = "eks"
			}
			if replica["self"] != "moimroom-db-"+env || replica["primary"] != "moimroom-db-gke" || replica["source"] != "moimroom-db-"+source {
				t.Fatalf("wrong roles: %v", replica)
			}
			bootstrap := spec["bootstrap"].(map[string]any)
			if env == "gke" {
				if bootstrap["initdb"] == nil {
					t.Fatal("initial primary has no initdb")
				}
			} else if bootstrap["pg_basebackup"].(map[string]any)["source"] != "moimroom-db-gke" {
				t.Fatalf("replica must bootstrap from primary: %v", bootstrap)
			}
			external := spec["externalClusters"].([]any)
			if len(external) != 3 {
				t.Fatalf("externalClusters: %v", external)
			}
			for _, member := range []string{"gke", "eks", "onprem"} {
				found := false
				for _, entry := range external {
					c := entry.(map[string]any)
					if c["name"] == "moimroom-db-"+member {
						found = true
						if member != env && c["connectionParameters"].(map[string]any)["host"] != "moimroom-db-to-"+member+".moimroom.svc.cluster.local" {
							t.Fatalf("wrong peer endpoint: %v", c)
						}
					}
				}
				if !found {
					t.Fatalf("missing member %s", member)
				}
				if member == env {
					continue
				}
				var service corev1.Service
				decode(t, docs, "Service/moimroom-db-to-"+member, &service)
				if service.Annotations["tailscale.com/tailnet-fqdn"] != "hop-db-"+member+"-moimroom.tail567fe8.ts.net" {
					t.Fatalf("wrong tailnet target: %v", service.Annotations)
				}
			}
			var policy networkingv1.NetworkPolicy
			decode(t, docs, "NetworkPolicy/moimroom-db-replication", &policy)
			if len(policy.Spec.Egress) != 2 {
				t.Fatalf("replication needs both remote proxies: %v", policy.Spec.Egress)
			}
			var ns corev1.Namespace
			decode(t, docs, "Namespace/moimroom", &ns)
			if ns.Labels["hopcharlie.dev/db-topology-members"] != "3" {
				t.Fatal("namespace has no three-member marker")
			}
		})
	}
}

func TestRejectInvalidTopologyMembers(t *testing.T) {
	for _, extra := range []string{
		", members: []", ", members: [gke]", ", members: [gke, eks, onprem, fourth]",
		", members: [gke, eks, eks]", ", members: [eks, onprem]", ", members: [gke, onprem]",
		", members: [gke, eks]", ", members: [gke, eks, onprem], peerPresent: false",
	} {
		t.Run(extra, func(t *testing.T) {
			_, err := render(t, appChart, "moimroom", writeValues(t, fmt.Sprintf(topologyValues, "onprem", extra)))
			if err == nil {
				t.Fatal("invalid members rendered")
			}
		})
	}
	// The second remote Service must receive the same collision protection
	// that the old single-peer helper applied only to its first remote.
	values := strings.Replace(fmt.Sprintf(topologyValues, "gke", ", members: [gke, eks, onprem]"), "name: web", "name: db-to-onprem", 1)
	if _, err := render(t, appChart, "moimroom", writeValues(t, values)); err == nil || !strings.Contains(err.Error(), "also an app service") {
		t.Fatalf("second remote Service collision: %v", err)
	}
}
