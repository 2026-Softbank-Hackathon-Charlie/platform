package discover

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// Locator finds the VM a public IPv4 address belongs to. A nil Instance and
// no error means the cloud has no instance with it.
type Locator interface {
	FindInstance(ctx context.Context, ip string) (*facts.Instance, error)
}

// EC2API is the part of the EC2 client discovery and connecting use.
type EC2API interface {
	DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, opts ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	DescribeNetworkInterfaces(ctx context.Context, in *ec2.DescribeNetworkInterfacesInput, opts ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// SSMInfoAPI is the SSM call that says whether Run Command reaches an
// instance.
type SSMInfoAPI interface {
	DescribeInstanceInformation(ctx context.Context, in *ssm.DescribeInstanceInformationInput, opts ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error)
}

// AWSRegion is one region's clients.
type AWSRegion struct {
	Region string
	EC2    EC2API
	SSM    SSMInfoAPI
}

// AWSLocator looks in each region in order (ADR 0042: DescribeAddresses,
// then DescribeNetworkInterfaces, then DescribeInstances).
type AWSLocator struct {
	Regions []AWSRegion
}

// Tags that make an instance part of something the platform must not take
// (blockers vm_in_autoscaling_group, vm_is_eks_node).
const (
	tagASG = "aws:autoscaling:groupName"
	tagEKS = "eks:cluster-name"
)

// FindInstance implements Locator.
func (l AWSLocator) FindInstance(ctx context.Context, ip string) (*facts.Instance, error) {
	for _, r := range l.Regions {
		id, alloc, err := instanceByIP(ctx, r.EC2, ip)
		if err != nil {
			return nil, fmt.Errorf("aws %s: %w", r.Region, err)
		}
		if id == "" {
			continue
		}
		out, err := r.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
		if err != nil {
			return nil, fmt.Errorf("aws %s: describe instance %s: %s", r.Region, id, awsCode(err))
		}
		for _, res := range out.Reservations {
			for _, in := range res.Instances {
				inst := &facts.Instance{Cloud: facts.CloudAWS, ID: aws.ToString(in.InstanceId), Region: r.Region,
					Type: string(in.InstanceType), PublicIP: ip, ElasticIP: alloc, Tags: map[string]string{}}
				if in.Placement != nil {
					inst.Zone = aws.ToString(in.Placement.AvailabilityZone)
				}
				for _, t := range in.Tags {
					k, v := aws.ToString(t.Key), aws.ToString(t.Value)
					inst.Tags[k] = v
					switch {
					case k == tagASG:
						inst.AutoScalingGroup = v
					case k == tagEKS:
						inst.EKSCluster = v
					case strings.HasPrefix(k, "kubernetes.io/cluster/") && inst.EKSCluster == "":
						inst.EKSCluster = strings.TrimPrefix(k, "kubernetes.io/cluster/")
					}
				}
				if r.SSM != nil {
					managed, err := ssmManaged(ctx, r.SSM, inst.ID)
					if err != nil {
						return nil, fmt.Errorf("aws %s: %w", r.Region, err)
					}
					inst.SSMManaged = managed
				}
				return inst, nil
			}
		}
	}
	return nil, nil
}

// instanceByIP returns the instance whose public address is ip and, for an
// Elastic IP, its allocation id.
func instanceByIP(ctx context.Context, c EC2API, ip string) (id, alloc string, err error) {
	addrs, err := c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: []ec2types.Filter{{Name: aws.String("public-ip"), Values: []string{ip}}}})
	if err != nil {
		return "", "", fmt.Errorf("describe addresses: %s", awsCode(err))
	}
	for _, a := range addrs.Addresses {
		if aws.ToString(a.InstanceId) != "" {
			return aws.ToString(a.InstanceId), aws.ToString(a.AllocationId), nil
		}
		alloc = aws.ToString(a.AllocationId)
	}
	enis, err := c.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{{Name: aws.String("association.public-ip"), Values: []string{ip}}}})
	if err != nil {
		return "", "", fmt.Errorf("describe network interfaces: %s", awsCode(err))
	}
	for _, e := range enis.NetworkInterfaces {
		if e.Attachment != nil && aws.ToString(e.Attachment.InstanceId) != "" {
			return aws.ToString(e.Attachment.InstanceId), alloc, nil
		}
	}
	// A load balancer's or NAT gateway's interface has the address but no
	// instance.
	return "", "", nil
}

func ssmManaged(ctx context.Context, c SSMInfoAPI, id string) (bool, error) {
	out, err := c.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
		Filters: []ssmtypes.InstanceInformationStringFilter{{Key: aws.String("InstanceIds"), Values: []string{id}}}})
	if err != nil {
		return false, fmt.Errorf("describe instance information: %s", awsCode(err))
	}
	for _, i := range out.InstanceInformationList {
		if aws.ToString(i.InstanceId) == id && i.PingStatus == ssmtypes.PingStatusOnline {
			return true, nil
		}
	}
	return false, nil
}

func awsCode(err error) string {
	var ae interface {
		ErrorCode() string
		ErrorMessage() string
	}
	if errors.As(err, &ae) {
		return ae.ErrorCode() + ": " + ae.ErrorMessage()
	}
	return err.Error()
}
