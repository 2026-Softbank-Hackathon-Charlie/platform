package recommend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Prices reads only the small, regional file used by AWS's public price page.
// It never uses a cloud credential, account bill, Spot or commitment API.
// Google Catalog needs an operator-provided API key; without it no Google
// price is invented or inferred from the default Iowa table.
type Prices struct {
	mu      sync.Mutex
	client  *http.Client
	entries map[string]priceEntry
}
type priceEntry struct {
	until       time.Time
	summary     string
	source      Source
	price       float64
	publication time.Time
}

var awsLocations = map[string]string{
	"ap-northeast-2": "Asia%20Pacific%20%28Seoul%29",
	"ap-northeast-1": "Asia%20Pacific%20%28Tokyo%29",
	"us-east-1":      "US%20East%20%28N.%20Virginia%29",
}

func NewPrices() *Prices {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Prices{client: &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, entries: map[string]priceEntry{}}
}
func (p *Prices) Reference(ctx context.Context, region string, now time.Time) (string, Source) {
	location, ok := awsLocations[region]
	if !ok {
		return "", Source{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if previous, ok := p.entries[region]; ok && now.Before(previous.until) {
		return previous.summary, previous.source
	}
	endpoint := "https://b0.p.awsstatic.com/pricing/2.0/meteredUnitMaps/ec2/USD/current/ec2-ondemand-without-sec-sel/" + location + "/Linux/index.json"
	expectedLocation, _ := url.PathUnescape(location)
	next, err := p.fetch(ctx, endpoint, expectedLocation, now)
	if err != nil {
		// Failures also have a cooldown; repeated page entries cannot hammer a
		// provider or silently reuse an expired price as a fresh quote.
		p.entries[region] = priceEntry{until: now.Add(15 * time.Minute)}
		return "", Source{}
	}
	if old := p.entries[region]; old.price > 0 && next.publication.After(old.publication) {
		delta := (next.price - old.price) / old.price * 100
		if math.Abs(delta) >= .01 {
			next.summary += fmt.Sprintf(" · 직전 공시 대비 %+.1f%%", delta)
		}
	}
	p.entries[region] = next
	return next.summary, next.source
}
func (p *Prices) fetch(ctx context.Context, endpoint, expectedLocation string, now time.Time) (priceEntry, error) {
	if !validPriceURL(endpoint) {
		return priceEntry{}, errors.New("price source unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return priceEntry{}, err
	}
	res, err := p.client.Do(req)
	if err != nil {
		return priceEntry{}, errors.New("price source unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return priceEntry{}, errors.New("price source unavailable")
	}
	var data struct {
		Manifest struct {
			Published time.Time `json:"hawkFilePublicationDate"`
			Currency  string    `json:"currencyCode"`
			Source    string    `json:"source"`
		} `json:"manifest"`
		Regions map[string]map[string]map[string]string `json:"regions"`
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return priceEntry{}, errors.New("price source too large")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	if d.Decode(&data) != nil || d.Decode(new(any)) != io.EOF || data.Manifest.Currency != "USD" || data.Manifest.Source != "ec2-ondemand-without-sec-sel" || data.Manifest.Published.IsZero() || data.Manifest.Published.After(now.Add(5*time.Minute)) || now.Sub(data.Manifest.Published) > 7*24*time.Hour {
		return priceEntry{}, errors.New("price source invalid or stale")
	}
	if len(data.Regions) != 1 {
		return priceEntry{}, errors.New("price region ambiguous")
	}
	var found *priceEntry
	for location, offers := range data.Regions {
		if location != expectedLocation {
			return priceEntry{}, errors.New("price region mismatch")
		}
		for _, offer := range offers {
			if offer["Instance Type"] != "m7i.large" || offer["Operating System"] != "Linux" || offer["Pre Installed S/W"] != "NA" {
				continue
			}
			if offer["Location"] != expectedLocation || found != nil {
				return priceEntry{}, errors.New("price offer ambiguous")
			}
			price, err := strconv.ParseFloat(offer["price"], 64)
			if err != nil || price <= 0 || price > 100 || math.IsInf(price, 0) || math.IsNaN(price) {
				return priceEntry{}, errors.New("price invalid")
			}
			source := Source{Title: "AWS m7i.large Linux 종량제 공시 · 컴퓨트만, 총비용 아님", URL: endpoint, ObservedAt: &data.Manifest.Published}
			entry := priceEntry{until: now.Add(24 * time.Hour), summary: fmt.Sprintf("AWS m7i.large 참고 단가는 시간당 $%.4f이며 별도 비용은 포함하지 않습니다.", price), source: source, price: price, publication: data.Manifest.Published}
			found = &entry
		}
	}
	if found != nil {
		return *found, nil
	}
	return priceEntry{}, errors.New("reference instance unavailable")
}
