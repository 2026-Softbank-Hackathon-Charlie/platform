package job

import (
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// report is the API's report of the facts' report.
func report(r facts.Report) apiv1.OnboardingReport {
	out := apiv1.OnboardingReport{Blockers: findings(r.Blockers), Warnings: findings(r.Warnings)}
	if d := r.Drift; d != nil {
		out.Drift = &apiv1.OnboardingDrift{Head: d.Head}
		if d.RunningRevision != "" {
			out.Drift.RunningRevision = ptr(d.RunningRevision)
		}
		if d.LastDeployCommit != "" {
			out.Drift.LastDeployCommit = ptr(d.LastDeployCommit)
		}
	}
	if h := r.Hold; h != nil {
		out.HoldSeconds, out.HoldBudgetSeconds = ptr(float32(h.TotalSeconds)), ptr(float32(h.BudgetSeconds))
	}
	return out
}

func findings(in []facts.Finding) []apiv1.OnboardingFinding {
	out := make([]apiv1.OnboardingFinding, 0, len(in))
	for _, f := range in {
		o := apiv1.OnboardingFinding{Code: apiv1.OnboardingFindingCode(f.Code), Message: f.Message}
		if f.Subject != "" {
			o.Subject = ptr(f.Subject)
		}
		out = append(out, o)
	}
	return out
}

func optional[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

// summary is what the console shows of the facts.
func summary(f facts.Facts) *apiv1.OnboardingFactsSummary {
	s := &apiv1.OnboardingFactsSummary{}
	if g := f.GitHub; g != nil {
		s.DefaultBranch, s.Head = optional(g.DefaultBranch), optional(g.Head)
		var deploy []string
		for _, w := range g.Workflows {
			if w.DeploysToVM {
				deploy = append(deploy, w.Path)
			}
		}
		if deploy != nil {
			s.DeployWorkflows = &deploy
		}
	}
	if i := f.Instance; i != nil {
		c := apiv1.OnboardingFactsSummaryCloud(i.Cloud)
		s.Cloud, s.InstanceId, s.Region, s.PublicIp, s.SsmManaged = &c, optional(i.ID), optional(i.Region), optional(i.PublicIP), ptr(i.SSMManaged)
	}
	if v := f.VM; v != nil {
		s.ComposeProject = optional(v.Project)
		var names []string
		for _, c := range v.Containers {
			names = append(names, c.Name)
		}
		if names != nil {
			s.Containers = &names
		}
	}
	if d := f.DB; d != nil {
		s.DbDatabase, s.DbOwner, s.DbServerVersionNum = optional(d.Database), optional(d.Owner), optional(d.ServerVersionNum)
		if d.Libc != "" {
			l := apiv1.OnboardingFactsSummaryDbLibc(d.Libc)
			s.DbLibc = &l
		}
		if f.VM != nil {
			for _, c := range f.VM.Containers {
				if c.Name == d.Container {
					s.DbImage = optional(c.Image)
				}
			}
		}
		var size int64
		for _, db := range d.Databases {
			if db.Name == d.Database {
				size = db.SizeBytes
			}
		}
		s.DbSizeBytes = optional(size)
	}
	if st := f.Storage; st != nil {
		s.Bucket, s.ObjectCount = optional(st.Bucket), optional(st.ObjectCount)
	}
	if t := f.Target; t != nil {
		s.Image, s.StorageSize = optional(t.Image), optional(t.StorageSize)
		if len(t.Reindex) > 0 {
			s.Reindex = &t.Reindex
		}
	}
	return s
}
