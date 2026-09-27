package ratelimit

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestDiscordMajor(t *testing.T) {
	for path, want := range map[string]string{
		"/channels/123/messages":      "channels/123",
		"/guilds/456/members/7":       "guilds/456",
		"/webhooks/1/tok-en/messages": "webhooks/1/tok-en",
		"/webhooks/1":                 "webhooks/1",
		"/users/@me":                  "",
	} {
		if got := DiscordMajor(path); got != want {
			t.Errorf("%s: got %q want %q", path, got, want)
		}
	}
}

func TestDiscordBucketWaitsForReset(t *testing.T) {
	d := NewDiscord(50)
	ctx := context.Background()
	route, err := d.Acquire(ctx, "c", "POST", "/channels/1/messages")
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset-After", "0.2")
	h.Set("X-RateLimit-Bucket", "hash")
	d.Update("c", route, "/channels/1/messages", h)

	start := time.Now()
	if _, err := d.Acquire(ctx, "c", "POST", "/channels/1/messages"); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Fatalf("did not wait for bucket reset (%v)", waited)
	}

	start = time.Now()
	if _, err := d.Acquire(ctx, "c", "POST", "/channels/2/messages"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("a different channel shared the exhausted bucket")
	}
	start = time.Now()
	if _, err := d.Acquire(ctx, "other", "POST", "/channels/1/messages"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("a different credential shared the exhausted bucket")
	}
}

func TestDiscordGlobalLimit(t *testing.T) {
	d := NewDiscord(5)
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 8; i++ {
		if _, err := d.Acquire(ctx, "c", "GET", "/users/@me"); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 450*time.Millisecond {
		t.Fatalf("8 requests at 5 rps finished in %v", elapsed)
	}
}

func TestDiscordBlockGlobalAndCancel(t *testing.T) {
	d := NewDiscord(50)
	d.BlockGlobal("c", time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := d.Acquire(ctx, "c", "GET", "/users/@me"); err == nil {
		t.Fatal("expected context error while globally blocked")
	}
}

func TestDiscordRetryAfter(t *testing.T) {
	h := http.Header{}
	if got := DiscordRetryAfter(h, []byte(`{"retry_after":1.5}`)); got != 1500*time.Millisecond {
		t.Fatalf("body: %v", got)
	}
	h.Set("Retry-After", "2")
	if got := DiscordRetryAfter(h, nil); got != 2*time.Second {
		t.Fatalf("header: %v", got)
	}
	if got := DiscordRetryAfter(http.Header{}, []byte("nope")); got != time.Second {
		t.Fatalf("default: %v", got)
	}
}

func TestGitHubRetryAfterOnlyOnLimitStatuses(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "3")
	if _, ok := GitHubRetryAfter(202, h); ok {
		t.Fatal("retried a 202")
	}
	if d, ok := GitHubRetryAfter(403, h); !ok || d != 3*time.Second {
		t.Fatalf("403: %v %v", d, ok)
	}
}

func TestWindow(t *testing.T) {
	w := NewWindow()
	for i := 0; i < 2; i++ {
		if ok, _ := w.Allow("s", 2, time.Minute); !ok {
			t.Fatal("rejected within limit")
		}
	}
	if ok, reset := w.Allow("s", 2, time.Minute); ok || reset <= 0 {
		t.Fatal("allowed over limit")
	}
	if remaining, _ := w.Status("s", 2); remaining != 0 {
		t.Fatalf("remaining %d", remaining)
	}
}
