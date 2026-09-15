package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/lib/pq"
)

type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	SSLMode  string

	// PasswordSecretARN, if set, is an AWS Secrets Manager secret ARN
	// holding the CURRENT password — set this for a database whose
	// password can change without a deploy (e.g. RDS's own
	// manage_master_user_password rotation, which runs on its own
	// schedule with no way to push the new value into an already-running
	// container). Password above is still used as the seed value so
	// startup costs zero extra Secrets Manager calls; ARN-based refresh
	// only kicks in periodically after that. Leave empty for a static
	// password (e.g. local dev) — behavior is then identical to before
	// this field existed.
	PasswordSecretARN string
}

type Client struct {
	db *sql.DB
}

// passwordSecretTTL bounds how stale a cached password can be after a
// rotation — short enough that a rotated RDS secret (rotates on a
// multi-day schedule, not urgently) is picked up well within one
// support conversation, long enough that it doesn't meaningfully add to
// Secrets Manager's request volume or cost.
const passwordSecretTTL = 60 * time.Second

// secretFetcher is the one method passwordCache needs from
// *secretsmanager.Client — narrowed to an interface so tests can supply
// a fake instead of making real AWS calls.
type secretFetcher interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// passwordCache holds the last-known-good password plus when it was
// fetched, refreshing from Secrets Manager only once the TTL has
// elapsed — this is what lets a long-running process notice a rotation
// without needing a restart, confirmed necessary live (2026-09-15): a
// task running since before a scheduled RDS password rotation kept
// authenticating with the pre-rotation password indefinitely, since a
// plain sql.Open DSN string is fixed at connection-pool-creation time
// and ECS `secrets` injection only resolves once, at container start.
type passwordCache struct {
	mu       sync.Mutex
	current  string
	fetchAt  time.Time
	secretID string
	client   secretFetcher
	ttl      time.Duration
	now      func() time.Time
}

func newPasswordCache(ctx context.Context, secretARN, seed string) (*passwordCache, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config for password rotation: %w", err)
	}
	return &passwordCache{
		current:  seed,
		fetchAt:  time.Now(), // seed counts as "just fetched" — no cold-start API call
		secretID: secretARN,
		client:   secretsmanager.NewFromConfig(awsCfg),
		ttl:      passwordSecretTTL,
		now:      time.Now,
	}, nil
}

// get returns the current password, refreshing from Secrets Manager
// first if the cached value is older than the TTL. A refresh failure
// falls back to the last-known value rather than failing the connection
// outright — Secrets Manager being briefly unreachable should not
// itself take the database down when the cached credential might still
// work.
func (p *passwordCache) get(ctx context.Context) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.now().Sub(p.fetchAt) < p.ttl {
		return p.current
	}

	out, err := p.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: &p.secretID,
	})
	if err != nil {
		// Keep using the cached value; try again on the next connection
		// attempt rather than blocking this one on a transient AWS issue.
		p.fetchAt = p.now()
		return p.current
	}

	if out.SecretString != nil {
		p.current = extractPassword(*out.SecretString)
	}
	p.fetchAt = p.now()
	return p.current
}

// extractPassword handles both secret shapes this can point at: an
// RDS-managed secret (manage_master_user_password = true) stores a JSON
// blob ({"username":...,"password":...}, same as ECS's own ":password::"
// selector unpacks at injection time — but the plain GetSecretValue API
// has no equivalent selector, so a caller must parse it), while a
// hand-created secret might just be the bare password string. Tried as
// JSON first; any string that doesn't parse as the RDS shape is used
// as-is.
func extractPassword(secretString string) string {
	var rdsSecret struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(secretString), &rdsSecret); err == nil && rdsSecret.Password != "" {
		return rdsSecret.Password
	}
	return secretString
}

// rotatingConnector implements database/sql/driver.Connector, building
// the DSN fresh on every new physical connection instead of once at
// sql.Open time — sql.Open bakes its dsn string in permanently, which is
// exactly what makes a plain *sql.DB blind to a rotated password no
// matter how often idle connections get recycled.
type rotatingConnector struct {
	pqDriver driver.Driver
	dsnBase  string // everything except "password=..."
	passwd   *passwordCache
}

func (c *rotatingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	dsn := fmt.Sprintf("%s password=%s", c.dsnBase, quoteDSNValue(c.passwd.get(ctx)))
	return c.pqDriver.Open(dsn)
}

// quoteDSNValue escapes a value for libpq's conninfo "key='value'"
// format specifically — NOT the same rules as a SQL string literal.
// conninfo parsing (see lib/pq's parseOpts) treats a quoted value as
// single-quote-delimited with backslash as its own escape character
// (\\  and \' ), unrelated to SQL's doubled-quote escaping. Using
// pq.QuoteLiteral (SQL-literal quoting) here would silently corrupt any
// password containing a backslash or single quote.
func quoteDSNValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

func (c *rotatingConnector) Driver() driver.Driver { return c.pqDriver }

func NewClient(cfg Config) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var db *sql.DB

	if cfg.PasswordSecretARN == "" {
		// Unchanged from before this field existed: one static DSN,
		// exactly what local dev and any environment without a rotating
		// secret should keep doing.
		dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
			cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DBName, cfg.SSLMode)
		var err error
		db, err = sql.Open("postgres", dsn)
		if err != nil {
			return nil, fmt.Errorf("failed to open database: %w", err)
		}
	} else {
		cache, err := newPasswordCache(ctx, cfg.PasswordSecretARN, cfg.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to set up password rotation: %w", err)
		}
		dsnBase := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=%s",
			cfg.Host, cfg.Port, cfg.User, cfg.DBName, cfg.SSLMode)
		db = sql.OpenDB(&rotatingConnector{
			pqDriver: &pq.Driver{},
			dsnBase:  dsnBase,
			passwd:   cache,
		})
	}

	// Connection pool settings
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &Client{db: db}, nil
}

func (c *Client) Close() error {
	return c.db.Close()
}

func (c *Client) DB() *sql.DB {
	return c.db
}

func (c *Client) Ping(ctx context.Context) error {
	return c.db.PingContext(ctx)
}
