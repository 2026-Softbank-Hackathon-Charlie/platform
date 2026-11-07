package fileguard

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sync"
)

const MaxClassificationEntries = 4096

// ClassificationCache contains only content classification, never file bytes,
// keys, transfer approval or a dynamic authorization/probe result. A miss or an
// eviction simply runs the original inspector. It is private to one Gate.
type ClassificationCache struct {
	mu       sync.Mutex
	capacity int
	items    map[[32]byte]*list.Element
	lru      list.List
}

type classification struct {
	key      [32]byte
	labels   []string
	findings map[string]int
	expanded int64
	members  int
}

func NewClassificationCache(capacity int) *ClassificationCache {
	if capacity <= 0 || capacity > MaxClassificationEntries {
		capacity = MaxClassificationEntries
	}
	return &ClassificationCache{capacity: capacity, items: map[[32]byte]*list.Element{}}
}

func (c *ClassificationCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func copyClassification(v classification) classification {
	v.labels = slices.Clone(v.labels)
	findings := make(map[string]int, len(v.findings))
	for k, n := range v.findings {
		findings[k] = n
	}
	v.findings = findings
	return v
}

func (c *ClassificationCache) get(key [32]byte) (classification, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return classification{}, false
	}
	c.lru.MoveToFront(e)
	return copyClassification(e.Value.(classification)), true
}

func (c *ClassificationCache) put(v classification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[v.key]; ok {
		e.Value = copyClassification(v)
		c.lru.MoveToFront(e)
		return
	}
	c.items[v.key] = c.lru.PushFront(copyClassification(v))
	if len(c.items) > c.capacity {
		e := c.lru.Back()
		delete(c.items, e.Value.(classification).key)
		c.lru.Remove(e)
	}
}

type classificationMemo struct {
	cache        *ClassificationCache
	policy       [32]byte
	hits, misses int
}

// binding is a SHA256 of trusted, direction-normalized app/endpoint/policy
// identity, supplied by the Gate. The source's entire review map, binary label,
// rules and resource limits also participate, so a reversed move only hits if
// its actual classification policy is identical.
func newClassificationMemo(cache *ClassificationCache, binding string, reviews []Review, binary string) *classificationMemo {
	if cache == nil || len(binding) != 64 {
		return nil
	}
	if _, err := hex.DecodeString(binding); err != nil {
		return nil
	}
	policy := map[string]string{}
	for _, r := range reviews {
		if !ValidReview(r) || policy[r.SHA256] != "" {
			return nil
		}
		policy[r.SHA256] = r.Label
	}
	raw, _ := json.Marshal(struct {
		Binding, Rules, Binary  string
		Reviews                 map[string]string
		Object, Total, Expanded int64
		Objects, Depth          int
	}{binding, RulesVersion, binary, policy, MaxObjectBytes, MaxTotalBytes, MaxExpandedBytes, MaxObjects, MaxArchiveDepth})
	return &classificationMemo{cache: cache, policy: sha256.Sum256(raw)}
}

func (m *classificationMemo) inspect(ctx context.Context, s *scanner, data []byte, digest string) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ErrRead
	}
	if m == nil {
		return s.inspectDigest(ctx, data, 0, digest)
	}
	key := sha256.Sum256(append(m.policy[:len(m.policy):len(m.policy)], []byte(digest)...))
	if v, ok := m.cache.get(key); ok {
		if ctx.Err() != nil {
			return nil, ErrRead
		}
		m.hits++
		// Limits apply cumulatively to this scan, even when a compressed object
		// was previously inspected. Reusing labels cannot bypass a ZIP budget.
		if v.expanded > MaxExpandedBytes-s.expanded || v.members > MaxObjects-s.members {
			return nil, ErrLimit
		}
		s.expanded += v.expanded
		s.members += v.members
		for code, n := range v.findings {
			s.findings[code] += n
		}
		return v.labels, nil
	}
	m.misses++
	before := copyClassification(classification{findings: s.findings})
	expanded, members := s.expanded, s.members
	labels, err := s.inspectDigest(ctx, data, 0, digest)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ErrRead
	}
	findings := map[string]int{}
	for code, n := range s.findings {
		if delta := n - before.findings[code]; delta > 0 {
			findings[code] = delta
		}
	}
	m.cache.put(classification{key: key, labels: labels, findings: findings, expanded: s.expanded - expanded, members: s.members - members})
	return labels, nil
}
