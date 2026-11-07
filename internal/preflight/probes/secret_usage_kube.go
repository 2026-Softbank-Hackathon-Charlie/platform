package probes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"k8s.io/client-go/rest"
)

// ServiceUsageFetcher never dials an app-supplied URL. The trusted factory
// selects the registered cluster; service and namespace are server policy.
func ServiceUsageFetcher(factory func(string) (rest.Interface, error)) UsageFetch {
	return func(ctx context.Context, target parity.Target, probe SecretUsage, challenge string) ([]byte, error) {
		if !parity.ValidTarget(target) || !simpleID.MatchString(probe.Service) || probe.Port < 1 || probe.Port > 65535 {
			return nil, ErrInvalidConfig
		}
		client, err := factory(target.Environment)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		stream, err := client.Get().Namespace(target.Namespace).Resource("services").Name(fmt.Sprintf("http:%s:%d", probe.Service, probe.Port)).SubResource("proxy").Suffix("internal", "secret-usage").Param("challenge", challenge).Stream(ctx)
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		body, err := io.ReadAll(io.LimitReader(stream, 8193))
		if len(body) > 8192 {
			return nil, errors.New("secret usage response too large")
		}
		return body, err
	}
}
