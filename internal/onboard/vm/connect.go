package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// DisconnectDocument is the SSM document that undoes HopCharlie-Connect
// (infra disconnect.sh): parameters dbContainer and appLabel.
const DisconnectDocument = "HopCharlie-Disconnect"

// ConnectParams are HopCharlie-Connect's parameters but the public key,
// which Connect makes.
type ConnectParams struct {
	DBContainer      string
	Network          string
	Subnet           string
	TailscaleAuthKey string
	Hostname         string // onboard.TailnetHost(app)
	App              string
}

// Change is one line of connect.sh or disconnect.sh: an
// apiv1.OnboardingLegacyChange, with New false when an earlier run had
// already made it.
type Change struct {
	apiv1.OnboardingLegacyChange
	New bool `json:"new"`
}

// Connected is what HopCharlie-Connect did.
type Connected struct {
	Changes []Change
	// ReplicationPassword is hopcharlie_replicator's new password, in
	// clear: the caller seals it and drops it.
	ReplicationPassword string
}

// lines reads the scripts' JSON lines: changes, and the result object.
func lines(stdout string) (changes []Change, result map[string]any, err error) {
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var probe map[string]any
		if json.Unmarshal([]byte(line), &probe) != nil {
			continue
		}
		if _, ok := probe["result"]; ok {
			result = probe
			continue
		}
		var c Change
		if err := json.Unmarshal([]byte(line), &c); err != nil || !c.Kind.Valid() {
			return changes, result, fmt.Errorf("a line of the script's output is not a known change: %.80s", line)
		}
		changes = append(changes, c)
	}
	return changes, result, nil
}

// Connect runs HopCharlie-Connect. On an error, the changes the script
// printed before it failed are still returned, for DELETE to undo.
func (r *Runner) Connect(ctx context.Context, instance string, p ConnectParams) (Connected, error) {
	key, err := NewKey()
	if err != nil {
		return Connected{}, err
	}
	id, err := r.Send(ctx, instance, onboard.ConnectDocument, map[string]string{
		"dbContainer": p.DBContainer, "network": p.Network, "subnet": p.Subnet,
		"tailscaleAuthKey": p.TailscaleAuthKey, "hostname": p.Hostname,
		"publicKey": key.PublicKey(), "appLabel": p.App,
	})
	if err != nil {
		return Connected{}, err
	}
	res, err := r.Wait(ctx, instance, id, "")
	if err != nil {
		return Connected{}, err
	}
	changes, result, perr := lines(res.Stdout)
	out := Connected{Changes: changes}
	if res.Failed() {
		return out, stepError(onboard.ConnectDocument, res)
	}
	if perr != nil {
		return out, perr
	}
	env, _ := result["replicationPassword"].(string)
	if result["result"] != "ok" || env == "" {
		return out, errors.New("HopCharlie-Connect printed no result")
	}
	pw, err := key.Open(env)
	if err != nil {
		return out, err
	}
	out.ReplicationPassword = string(pw)
	return out, nil
}

// Disconnect runs HopCharlie-Disconnect and returns what it removed, each
// with UndoneAt.
func (r *Runner) Disconnect(ctx context.Context, instance, dbContainer, app string) ([]apiv1.OnboardingLegacyChange, error) {
	id, err := r.Send(ctx, instance, DisconnectDocument, map[string]string{"dbContainer": dbContainer, "appLabel": app})
	if err != nil {
		return nil, err
	}
	res, err := r.Wait(ctx, instance, id, "")
	if err != nil {
		return nil, err
	}
	changes, _, perr := lines(res.Stdout)
	out := make([]apiv1.OnboardingLegacyChange, 0, len(changes))
	for _, c := range changes {
		if c.UndoneAt == nil {
			t := time.Now().UTC()
			c.UndoneAt = &t
		}
		out = append(out, c.OnboardingLegacyChange)
	}
	if res.Failed() {
		return out, stepError(DisconnectDocument, res)
	}
	return out, perr
}
