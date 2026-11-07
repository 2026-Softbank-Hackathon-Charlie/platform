package recommend

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"slices"
	"sort"
	"time"
)

type Application struct {
	Name, Revision string
	Environments   []string
	Current        *string
	Moving         bool
	Persistent     bool
	// Countries is the conservative intersection of declared source data
	// restrictions, not a fresh inventory or authorization verdict.
	Countries []string
}
type Environment struct {
	Name, Provider, Region string
	Country                string
	Ready                  bool
	ObservedAt             time.Time
}

// CostQuote is an explicitly entered full-workload estimate, never a guessed
// share of a VM or an invoice. Quotes must be renewed for a workload revision.
// Missing operating, storage, transfer, commitment or on-prem TCO facts cannot
// produce a saving. Public compute-only prices remain reference evidence.
type CostQuote struct {
	App                  string    `json:"app"`
	Environment          string    `json:"environment"`
	Region               string    `json:"region"`
	WorkloadRevision     string    `json:"workloadRevision"`
	MonthlyUSD           float64   `json:"monthlyUsd"`
	PeriodHours          int       `json:"periodHours"`
	ObservedAt           time.Time `json:"observedAt"`
	IncludesStorage      bool      `json:"includesStorage"`
	IncludesDataTransfer bool      `json:"includesDataTransfer"`
	IncludesOperations   bool      `json:"includesOperations"`
	IncludesPower        bool      `json:"includesPower"`
	IncludesAmortization bool      `json:"includesAmortization"`
	CommitmentReviewed   bool      `json:"commitmentReviewed"`
	CapacityReviewed     bool      `json:"capacityReviewed"`
	ResidencyReviewed    bool      `json:"residencyReviewed"`
}
type CostBook struct {
	Version int         `json:"version"`
	Quotes  []CostQuote `json:"quotes"`
}

func LoadCosts(path string) (*CostBook, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("recommendation cost estimates unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(raw) > 65536 {
		return nil, errors.New("invalid recommendation cost estimates")
	}
	var book CostBook
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&book) != nil || d.Decode(new(any)) != io.EOF || book.Version != 1 || len(book.Quotes) > 64 {
		return nil, errors.New("invalid recommendation cost estimates")
	}
	seen := map[string]bool{}
	for _, q := range book.Quotes {
		key := q.App + "\x00" + q.Environment
		if !label.MatchString(q.App) || !label.MatchString(q.Environment) || q.WorkloadRevision == "" || len(q.WorkloadRevision) > 128 || seen[key] || q.MonthlyUSD < 0 || math.IsNaN(q.MonthlyUSD) || math.IsInf(q.MonthlyUSD, 0) || q.MonthlyUSD > 1e9 || q.PeriodHours < 1 || q.PeriodHours > 744 || q.ObservedAt.IsZero() {
			return nil, errors.New("invalid recommendation cost estimates")
		}
		seen[key] = true
	}
	return &book, nil
}

func estimate(book *CostBook, app Application, env Environment, now time.Time) (CostQuote, bool) {
	if book == nil {
		return CostQuote{}, false
	}
	for _, q := range book.Quotes {
		if q.App != app.Name || q.Environment != env.Name || q.Region != env.Region || app.Revision == "" || q.WorkloadRevision != app.Revision {
			continue
		}
		valid := !q.ObservedAt.After(now.Add(30*time.Second)) && now.Sub(q.ObservedAt) <= 24*time.Hour && q.IncludesStorage && q.IncludesDataTransfer && q.IncludesOperations && q.CommitmentReviewed && q.CapacityReviewed && q.ResidencyReviewed
		if env.Provider == "onprem" {
			valid = valid && q.MonthlyUSD > 0 && q.IncludesPower && q.IncludesAmortization
		}
		return q, valid
	}
	return CostQuote{}, false
}

func recentEnvironment(e Environment, now time.Time) bool {
	return e.Ready && !e.ObservedAt.IsZero() && !e.ObservedAt.After(now.Add(30*time.Second)) && now.Sub(e.ObservedAt) <= 2*time.Minute
}
func str(value string) *string { return &value }

func candidateID(app, source, target string) string {
	raw, _ := json.Marshal([]string{app, source, target})
	sum := sha256.Sum256(raw)
	return "candidate-" + hex.EncodeToString(sum[:16])
}

// Build constructs candidate facts; only the sandbox AI chooses/ranks them.
// Readiness is not equivalent to spare memory or successful migration parity.
// All move recommendations still enter the normal Plan/preflight/approval flow.
func Build(apps []Application, envs []Environment, book *CostBook, reference map[string]string, now time.Time) []Candidate {
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	known := map[string]Environment{}
	for _, e := range envs {
		known[e.Name] = e
	}
	out := []Candidate{}
	for _, app := range apps {
		if len(out) >= MaxCandidates {
			break
		}
		if len(out) > 0 {
			seenApps := map[string]bool{}
			for _, c := range out {
				seenApps[c.App] = true
			}
			if !seenApps[app.Name] && len(seenApps) >= MaxApps {
				break
			}
		}
		if app.Current == nil || !slices.Contains(app.Environments, *app.Current) {
			out = append(out, Candidate{Item: Item{ID: candidateID(app.Name, "unknown", ""), App: app.Name, Title: app.Name + " 실행 위치 확인", Reasons: []string{"현재 쓰기 경로를 확인한 뒤 이전 위치를 비교합니다."}, Missing: []string{"current_environment_unobserved"}, Sources: []Source{}}, AllowedActions: []string{"stay", "insufficient"}})
			continue
		}
		source, ok := known[*app.Current]
		if !ok {
			continue
		}
		targets := slices.Clone(app.Environments)
		sort.Strings(targets)
		count := 0
		for _, name := range targets {
			if name == source.Name || len(out) >= MaxCandidates || count >= 2 {
				continue
			}
			target, exists := known[name]
			if !exists {
				continue
			}
			item := Item{ID: candidateID(app.Name, source.Name, name), App: app.Name, Source: str(source.Name), Target: str(name), Title: app.Name + " 이전 조건 비교", Reasons: []string{}, Missing: []string{}, Sources: policySources(source.Provider, target.Provider, now)}
			actions := []string{"stay", "insufficient"}
			ready := recentEnvironment(source, now) && recentEnvironment(target, now) && !app.Moving
			if app.Moving {
				item.Reasons = append(item.Reasons, "진행 중인 이전이 끝난 뒤 다음 위치를 검토합니다.")
				item.Missing = append(item.Missing, "migration_in_progress")
			} else if !ready {
				item.Reasons = append(item.Reasons, "현재·대상 환경의 최신 준비 상태를 먼저 확인합니다.")
				item.Missing = append(item.Missing, "environment_readiness_unknown")
			} else {
				item.Reasons = append(item.Reasons, "등록된 대상 환경이 응답합니다. 자원 여유는 별도 확인이 필요합니다.")
			}
			from, fromOK := estimate(book, app, source, now)
			to, toOK := estimate(book, app, target, now)
			countryDenied := app.Countries != nil && !slices.Contains(app.Countries, target.Country)
			if countryDenied {
				item.Reasons = append(item.Reasons, "등록 데이터 정책상 대상 국가로의 반출을 허용하지 않습니다.")
				item.Missing = append(item.Missing, "destination_country_restricted")
			}
			if ready && !countryDenied && fromOK && toOK && from.PeriodHours == to.PeriodHours {
				if from.MonthlyUSD > to.MonthlyUSD {
					saving := from.MonthlyUSD - to.MonthlyUSD
					item.EstimatedMonthlySavingsUSD = &saving
					item.Title = app.Name + " 비용 견적 비교"
					item.Reasons = append(item.Reasons, fmt.Sprintf("같은 운영시간의 입력 총비용 견적은 대상이 월 $%.2f 낮습니다.", saving))
					actions = append(actions, "move")
				} else {
					item.Reasons = append(item.Reasons, "입력된 총비용 견적상 현재 환경을 유지하는 편이 유리합니다.")
				}
				observed := from.ObservedAt
				if to.ObservedAt.Before(observed) {
					observed = to.ObservedAt
				}
				item.Sources = append(item.Sources, Source{Title: "운영자가 입력한 앱 전체 비용·용량·정책 검토 견적", URL: "https://console.hopcharlie.dev/", ObservedAt: &observed})
			} else {
				item.Reasons = append(item.Reasons, "약정·스토리지·데이터 반출을 포함한 총비용 견적을 확인합니다.")
				item.Missing = append(item.Missing, "comparable_total_cost_estimates", "capacity_preflight_required", "data_policy_preflight_required")
			}
			if target.Provider == "onprem" {
				item.Reasons = append(item.Reasons, "온프레미스 전력·장비 상각·운영비를 함께 비교합니다.")
			}
			if app.Persistent {
				item.Reasons = append(item.Reasons, "데이터 보관 위치·파일 분류가 허용하는 경로인지 검증해야 합니다.")
			}
			if value := reference[target.Name]; value != "" && len(item.Reasons) < 6 {
				item.Reasons = append(item.Reasons, value)
			}
			out = append(out, Candidate{Item: item, AllowedActions: actions})
			count++
		}
		if count == 0 && len(out) < MaxCandidates {
			out = append(out, Candidate{Item: Item{ID: candidateID(app.Name, *app.Current, "stay"), App: app.Name, Source: app.Current, Title: app.Name + " 현재 위치 유지", Reasons: []string{"아직 비교할 등록 대상 환경이 없습니다."}, Missing: []string{"target_environment_unregistered"}, Sources: []Source{}}, AllowedActions: []string{"stay", "insufficient"}})
		}
	}
	return out
}

func policySources(from, to string, now time.Time) []Source {
	out := []Source{}
	for _, provider := range []string{"aws", "gcp"} {
		if from != provider && to != provider {
			continue
		}
		if provider == "aws" {
			out = append(out, Source{Title: "AWS 종량제·약정·데이터 전송 요금", URL: "https://aws.amazon.com/ec2/pricing/on-demand/"})
		} else {
			out = append(out, Source{Title: "GCP 지역별 VM 요금·할인 적용 조건", URL: "https://cloud.google.com/products/compute/pricing/general-purpose"})
		}
	}
	// Vendor outlooks are reference signals, never cloud tariff multipliers.
	samsung := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	if now.Sub(samsung) < 90*24*time.Hour && now.After(samsung) {
		out = append(out, Source{Title: "Samsung 2026년 3분기 메모리 수급 전망 · 요금 단가 아님", URL: "https://news.samsung.com/nl/samsung-electronics-announces-third-quarter-2026-results", ObservedAt: &samsung})
	}
	nvidia := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	if now.Sub(nvidia) < 90*24*time.Hour && now.After(nvidia) {
		out = append(out, Source{Title: "NVIDIA FY2027 2분기 공급 전망 · 요금 단가 아님", URL: "https://investor.nvidia.com/files/content_files/TRANSCRIPT_-NVIDIA-Corp-NVDA-US-Q2-2027-Earnings-Call-26-August-2026-5_00-PM-ET.pdf", ObservedAt: &nvidia})
	}
	return out
}

func validPriceURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Host == "b0.p.awsstatic.com" && len(u.Path) > 0
}
