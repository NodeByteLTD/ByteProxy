package ratelimit

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const stripes = 256

type striped [stripes]sync.Mutex

func (s *striped) lock(key string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(key))
	return &s[h.Sum32()%stripes]
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func headerSeconds(h http.Header, name string) (time.Duration, bool) {
	v := h.Get(name)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) {
		return 0, false
	}
	return time.Duration(f * float64(time.Second)), true
}

type bucketState struct {
	remaining int
	resetAt   time.Time
	lastSeen  time.Time
}

type tokenBucket struct {
	tokens   float64
	last     time.Time
	blocked  time.Time
	lastSeen time.Time
}

type Discord struct {
	rps         float64
	mu          sync.Mutex
	global      map[string]*tokenBucket
	routeBucket map[string]string
	routeSeen   map[string]time.Time
	state       map[string]*bucketState
	locks       striped
}

var majorParam = regexp.MustCompile(`^/(?:(channels|guilds)/(\d+)|webhooks/(\d+)(?:/([^/?]+))?)`)

func NewDiscord(globalRPS int) *Discord {
	if globalRPS <= 0 {
		globalRPS = 50
	}
	return &Discord{
		rps:         float64(globalRPS),
		global:      map[string]*tokenBucket{},
		routeBucket: map[string]string{},
		routeSeen:   map[string]time.Time{},
		state:       map[string]*bucketState{},
	}
}

func DiscordRouteKey(method, path string) string {
	path, _, _ = strings.Cut(path, "?")
	return strings.ToUpper(method) + " " + path
}

func DiscordMajor(path string) string {
	m := majorParam.FindStringSubmatch(path)
	switch {
	case m == nil:
		return ""
	case m[1] != "":
		return m[1] + "/" + m[2]
	case m[4] != "":
		return "webhooks/" + m[3] + "/" + m[4]
	default:
		return "webhooks/" + m[3]
	}
}

func (d *Discord) bucketKey(cred, route, path string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if hash, ok := d.routeBucket[cred+"|"+route]; ok {
		return cred + "|" + hash + "|" + DiscordMajor(path)
	}
	return cred + "|" + route
}

func (d *Discord) waitGlobal(ctx context.Context, cred string) error {
	for {
		d.mu.Lock()
		now := time.Now()
		b := d.global[cred]
		if b == nil {
			b = &tokenBucket{tokens: d.rps, last: now}
			d.global[cred] = b
		}
		b.lastSeen = now
		var wait time.Duration
		if now.Before(b.blocked) {
			wait = b.blocked.Sub(now)
		} else {
			b.tokens = math.Min(d.rps, b.tokens+now.Sub(b.last).Seconds()*d.rps)
			b.last = now
			if b.tokens >= 1 {
				b.tokens--
				d.mu.Unlock()
				return nil
			}
			wait = time.Duration((1 - b.tokens) / d.rps * float64(time.Second))
		}
		d.mu.Unlock()
		if err := sleep(ctx, max(wait, time.Millisecond)); err != nil {
			return err
		}
	}
}

func (d *Discord) Acquire(ctx context.Context, cred, method, path string) (string, error) {
	route := DiscordRouteKey(method, path)
	if err := d.waitGlobal(ctx, cred); err != nil {
		return "", err
	}
	key := d.bucketKey(cred, route, path)
	lock := d.locks.lock(key)
	lock.Lock()
	defer lock.Unlock()

	d.mu.Lock()
	var wait time.Duration
	if st := d.state[key]; st != nil && st.remaining <= 0 {
		wait = time.Until(st.resetAt)
	}
	d.mu.Unlock()
	if err := sleep(ctx, wait); err != nil {
		return "", err
	}

	d.mu.Lock()
	if st := d.state[key]; st != nil {
		if time.Now().After(st.resetAt) {
			delete(d.state, key)
		} else {
			st.remaining--
		}
	}
	d.mu.Unlock()
	return route, nil
}

func (d *Discord) Update(cred, route, path string, h http.Header) {
	remaining, err1 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	resetAfter, ok := headerSeconds(h, "X-RateLimit-Reset-After")
	if err1 != nil || !ok {
		return
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	key := cred + "|" + route
	if hash := h.Get("X-RateLimit-Bucket"); hash != "" {
		d.routeBucket[cred+"|"+route] = hash
		d.routeSeen[cred+"|"+route] = now
		key = cred + "|" + hash + "|" + DiscordMajor(path)
	}
	d.state[key] = &bucketState{remaining: remaining, resetAt: now.Add(resetAfter), lastSeen: now}
}

func (d *Discord) BlockGlobal(cred string, wait time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.global[cred]
	if b == nil {
		b = &tokenBucket{tokens: 0, last: time.Now()}
		d.global[cred] = b
	}
	if until := time.Now().Add(wait); until.After(b.blocked) {
		b.blocked = until
	}
}

func DiscordRetryAfter(h http.Header, body []byte) time.Duration {
	if d, ok := headerSeconds(h, "Retry-After"); ok {
		return d
	}
	var payload struct {
		RetryAfter *float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.RetryAfter != nil && *payload.RetryAfter >= 0 {
		return time.Duration(*payload.RetryAfter * float64(time.Second))
	}
	return time.Second
}

func (d *Discord) Prune(olderThan time.Duration) {
	cutoff := time.Now().Add(-olderThan)
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, st := range d.state {
		if st.lastSeen.Before(cutoff) {
			delete(d.state, k)
		}
	}
	for k, seen := range d.routeSeen {
		if seen.Before(cutoff) {
			delete(d.routeSeen, k)
			delete(d.routeBucket, k)
		}
	}
	for k, b := range d.global {
		if b.lastSeen.Before(cutoff) && time.Now().After(b.blocked) {
			delete(d.global, k)
		}
	}
}

type resourceState struct {
	remaining int
	resetAt   time.Time
	lastSeen  time.Time
}

type GitHub struct {
	mu    sync.Mutex
	state map[string]*resourceState
	locks striped
}

func NewGitHub() *GitHub { return &GitHub{state: map[string]*resourceState{}} }

func GitHubResource(path string) string {
	switch {
	case strings.HasPrefix(path, "/search"):
		return "search"
	case strings.HasPrefix(path, "/graphql"):
		return "graphql"
	default:
		return "core"
	}
}

func (g *GitHub) Acquire(ctx context.Context, cred, path string) (string, error) {
	resource := GitHubResource(path)
	key := cred + "|" + resource
	lock := g.locks.lock(key)
	lock.Lock()
	defer lock.Unlock()

	g.mu.Lock()
	var wait time.Duration
	if st := g.state[key]; st != nil && st.remaining <= 0 {
		wait = time.Until(st.resetAt)
	}
	g.mu.Unlock()
	if err := sleep(ctx, wait); err != nil {
		return "", err
	}

	g.mu.Lock()
	if st := g.state[key]; st != nil {
		st.remaining--
	}
	g.mu.Unlock()
	return resource, nil
}

func (g *GitHub) Update(cred, fallback string, h http.Header) {
	remaining, err1 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	reset, err2 := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil {
		return
	}
	resource := h.Get("X-RateLimit-Resource")
	if resource == "" {
		resource = fallback
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state[cred+"|"+resource] = &resourceState{remaining: remaining, resetAt: time.Unix(reset, 0), lastSeen: time.Now()}
}

func GitHubRetryAfter(status int, h http.Header) (time.Duration, bool) {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return 0, false
	}
	return headerSeconds(h, "Retry-After")
}

func (g *GitHub) Prune(olderThan time.Duration) {
	cutoff := time.Now().Add(-olderThan)
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, st := range g.state {
		if st.lastSeen.Before(cutoff) {
			delete(g.state, k)
		}
	}
}

type windowState struct {
	count   int
	resetAt time.Time
}

type Window struct {
	mu    sync.Mutex
	state map[string]*windowState
}

func NewWindow() *Window { return &Window{state: map[string]*windowState{}} }

func (w *Window) Allow(key string, max int, window time.Duration) (bool, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	st := w.state[key]
	if st == nil || now.After(st.resetAt) {
		w.state[key] = &windowState{count: 1, resetAt: now.Add(window)}
		return true, 0
	}
	if st.count >= max {
		return false, st.resetAt.Sub(now)
	}
	st.count++
	return true, 0
}

func (w *Window) Status(key string, max int) (int, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.state[key]
	if st == nil || time.Now().After(st.resetAt) {
		return max, time.Time{}
	}
	return max - min(st.count, max), st.resetAt
}

func (w *Window) Remove(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.state, key)
}
