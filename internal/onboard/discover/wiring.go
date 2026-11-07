package discover

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/vm"
)

// RegionsEnv lists the AWS regions discovery looks in, comma-separated
// (default DefaultRegions).
const RegionsEnv = "PLATFORM_ONBOARD_AWS_REGIONS"

// DefaultRegions are the regions the team's account allows.
var DefaultRegions = []string{"ap-northeast-2", "ap-southeast-2"}

// Regions reads RegionsEnv.
func Regions(getenv func(string) string) []string {
	var out []string
	for _, r := range strings.Split(getenv(RegionsEnv), ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return DefaultRegions
	}
	return out
}

// NewAWSLocator returns a Locator over the regions with cfg's credentials.
func NewAWSLocator(cfg aws.Config, regions []string) AWSLocator {
	l := AWSLocator{}
	for _, r := range regions {
		c := cfg.Copy()
		c.Region = r
		l.Regions = append(l.Regions, AWSRegion{Region: r, EC2: ec2.NewFromConfig(c), SSM: ssm.NewFromConfig(c)})
	}
	return l
}

// SSMInspector is Inspector with an SSM client per region.
type SSMInspector struct{ Config aws.Config }

// Runner returns the document runner of a region.
func (s SSMInspector) Runner(region string) *vm.Runner {
	c := s.Config.Copy()
	c.Region = region
	return &vm.Runner{SSM: ssm.NewFromConfig(c)}
}

// Inspect implements Inspector.
func (s SSMInspector) Inspect(ctx context.Context, region, instance string) (*vm.Inspection, error) {
	return s.Runner(region).Inspect(ctx, instance)
}
