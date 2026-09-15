package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func TestQuoteDSNValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hunter2", `'hunter2'`},
		{"empty", "", `''`},
		{"single quote", "it's-a-pw", `'it\'s-a-pw'`},
		{"backslash", `pw\with\slashes`, `'pw\\with\\slashes'`},
		{"both", `o'\brien`, `'o\'\\brien'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := quoteDSNValue(tc.in)
			if got != tc.want {
				t.Fatalf("quoteDSNValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractPassword(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"RDS-managed JSON secret", `{"username":"teepin","password":"s3cr3t!"}`, "s3cr3t!"},
		{"bare password string", "s3cr3t!", "s3cr3t!"},
		{"JSON without a password field", `{"username":"teepin"}`, `{"username":"teepin"}`},
		{"not JSON at all", `not-json-{{{`, `not-json-{{{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractPassword(tc.in); got != tc.want {
				t.Fatalf("extractPassword(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// fakeSecretFetcher lets tests control exactly what GetSecretValue
// returns without making a real AWS call.
type fakeSecretFetcher struct {
	value string
	err   error
	calls int
}

func (f *fakeSecretFetcher) GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &secretsmanager.GetSecretValueOutput{SecretString: &f.value}, nil
}

// TestPasswordCache_ServesSeedWithoutAnAPICall proves the very first
// call costs zero Secrets Manager requests — the seed (whatever ECS
// already injected at container start) counts as freshly fetched, so a
// cold start never waits on an extra round trip just to get the same
// value ECS already resolved for free.
func TestPasswordCache_ServesSeedWithoutAnAPICall(t *testing.T) {
	fetcher := &fakeSecretFetcher{value: "should-not-be-seen"}
	now := time.Now()
	cache := &passwordCache{
		current: "seed-password", fetchAt: now, secretID: "arn:...",
		client: fetcher, ttl: time.Minute, now: func() time.Time { return now },
	}

	if got := cache.get(context.Background()); got != "seed-password" {
		t.Fatalf("get() = %q, want seed value %q", got, "seed-password")
	}
	if fetcher.calls != 0 {
		t.Fatalf("expected 0 Secrets Manager calls on a fresh cache, got %d", fetcher.calls)
	}
}

// TestPasswordCache_RefreshesOnceTTLElapses proves a rotated password
// is actually picked up once the cache goes stale — this is the whole
// point of the cache: without it, a long-running process would use a
// pre-rotation password forever.
func TestPasswordCache_RefreshesOnceTTLElapses(t *testing.T) {
	fetcher := &fakeSecretFetcher{value: "rotated-password"}
	clock := time.Now()
	cache := &passwordCache{
		current: "old-password", fetchAt: clock, secretID: "arn:...",
		client: fetcher, ttl: time.Minute, now: func() time.Time { return clock },
	}

	if got := cache.get(context.Background()); got != "old-password" {
		t.Fatalf("before TTL elapses: get() = %q, want %q", got, "old-password")
	}

	clock = clock.Add(2 * time.Minute) // advance the fake clock past the TTL
	if got := cache.get(context.Background()); got != "rotated-password" {
		t.Fatalf("after TTL elapses: get() = %q, want %q", got, "rotated-password")
	}
	if fetcher.calls != 1 {
		t.Fatalf("expected exactly 1 Secrets Manager call, got %d", fetcher.calls)
	}
}

// TestPasswordCache_FallsBackToCachedValueOnFetchError proves a
// Secrets Manager outage doesn't itself break the database connection
// — the cached (possibly still-valid) password keeps being served
// rather than surfacing the AWS error to every connection attempt.
func TestPasswordCache_FallsBackToCachedValueOnFetchError(t *testing.T) {
	fetcher := &fakeSecretFetcher{err: errors.New("throttled")}
	clock := time.Now()
	cache := &passwordCache{
		current: "still-good-password", fetchAt: clock, secretID: "arn:...",
		client: fetcher, ttl: time.Minute, now: func() time.Time { return clock },
	}

	clock = clock.Add(2 * time.Minute)
	if got := cache.get(context.Background()); got != "still-good-password" {
		t.Fatalf("get() = %q, want cached value preserved on fetch error: %q", got, "still-good-password")
	}
}

// TestPasswordCache_DoesNotHammerSecretsManagerOnRepeatedErrors proves
// a fetch failure still advances fetchAt — otherwise every single
// connection attempt during a Secrets Manager outage would retry the
// API call instead of waiting out the TTL like a successful fetch does.
func TestPasswordCache_DoesNotHammerSecretsManagerOnRepeatedErrors(t *testing.T) {
	fetcher := &fakeSecretFetcher{err: errors.New("throttled")}
	clock := time.Now()
	cache := &passwordCache{
		current: "still-good-password", fetchAt: clock, secretID: "arn:...",
		client: fetcher, ttl: time.Minute, now: func() time.Time { return clock },
	}

	clock = clock.Add(2 * time.Minute)
	cache.get(context.Background())
	cache.get(context.Background()) // immediately again, TTL not elapsed since the failed attempt

	if fetcher.calls != 1 {
		t.Fatalf("expected the second call within the TTL to skip Secrets Manager, got %d calls", fetcher.calls)
	}
}
