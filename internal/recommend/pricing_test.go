package recommend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type priceTransport func(*http.Request) (*http.Response, error)

func (f priceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func priceFixture(now time.Time, location, value string) string {
	return fmt.Sprintf(`{"manifest":{"hawkFilePublicationDate":%q,"currencyCode":"USD","source":"ec2-ondemand-without-sec-sel"},"regions":{%q:{"offer":{"Instance Type":"m7i.large","Operating System":"Linux","Pre Installed S/W":"NA","Location":%q,"price":%q}}}}`, now.Format(time.RFC3339), location, location, value)
}
func TestPublicPricesCacheAndExactRegion(t *testing.T) {
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	calls := 0
	body := priceFixture(now, "Asia Pacific (Seoul)", "0.12")
	p := NewPrices()
	p.client.Transport = priceTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "b0.p.awsstatic.com" || r.Header.Get("Authorization") != "" {
			t.Fatal("credential or untrusted provider request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	summary, source := p.Reference(context.Background(), "ap-northeast-2", now)
	if summary == "" || source.ObservedAt == nil || !strings.Contains(summary, "별도 비용은 포함하지 않습니다") {
		t.Fatal("valid compute-only public reference missing")
	}
	for _, part := range []string{"참고용", "Asia Pacific (Seoul)", "m7i.large Linux", "$0.1200/시간", "컴퓨트만", "앱 인스턴스 미확인"} {
		if !strings.Contains(source.Title, part) {
			t.Fatalf("missing price assumption %q: %s", part, source.Title)
		}
	}
	if utf8.RuneCountInString(source.Title) > 120 {
		t.Fatal("reference title exceeds API/CLI contract")
	}
	p.Reference(context.Background(), "ap-northeast-2", now.Add(time.Hour))
	if calls != 1 {
		t.Fatal("page refresh repeated public pricing fetch")
	}
	body = priceFixture(now.Add(25*time.Hour), "US East (N. Virginia)", "0.01")
	summary, source = p.Reference(context.Background(), "ap-northeast-2", now.Add(25*time.Hour))
	if summary != "" || source.URL != "" {
		t.Fatal("another region price accepted for Seoul")
	}
	p.Reference(context.Background(), "ap-northeast-2", now.Add(25*time.Hour+time.Minute))
	if calls != 2 {
		t.Fatal("failed provider fetch bypassed cooldown")
	}
	p.Reference(context.Background(), "asia-northeast3", now)
	if calls != 2 {
		t.Fatal("GCP rate guessed from AWS")
	}
}
func TestPublicPricesRejectStaleMalformedAndOversizedBodies(t *testing.T) {
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	for _, body := range []string{priceFixture(now.Add(-8*24*time.Hour), "Asia Pacific (Seoul)", "0.12"), priceFixture(now.Add(31*time.Second), "Asia Pacific (Seoul)", "0.12"), priceFixture(now, "Asia Pacific (Seoul)", "NaN"), priceFixture(now, "Asia Pacific (Seoul)", "0"), strings.Repeat(" ", 4<<20) + "{}"} {
		p := NewPrices()
		p.client.Transport = priceTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})
		if text, _ := p.Reference(context.Background(), "ap-northeast-2", now); text != "" {
			t.Fatal("unverified reference accepted")
		}
	}
}
