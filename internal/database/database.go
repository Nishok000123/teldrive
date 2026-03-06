package database

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	extraClausePlugin "github.com/WinterYukky/gorm-extra-clause-plugin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/tgdrive/teldrive/internal/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// makeResolverLookupFunc returns a LookupFunc that uses the given list of DNS
// server addresses (host:port).  The first server that responds wins.  If no
// server is responsive the last error is returned.
func makeResolverLookupFunc(servers []string) func(ctx context.Context, host string) ([]string, error) {
	const dnsDialTimeout = 5 * time.Second
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: dnsDialTimeout}
			var lastErr error
			for _, server := range servers {
				// Default to port 53 when no port is specified.
				if !strings.Contains(server, ":") {
					server = server + ":53"
				}
				conn, err := d.DialContext(ctx, "udp", server)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, fmt.Errorf("failed to connect to any DNS resolver (tried %v): %w", servers, lastErr)
		},
	}
	return resolver.LookupHost
}

func NewDatabase(ctx context.Context, cfg *config.DBConfig, logCfg *config.DBLoggingConfig, lg *zap.Logger) (*gorm.DB, error) {
	level, err := zapcore.ParseLevel(logCfg.Level)
	if err != nil {
		level = zapcore.InfoLevel
	}

	var db *gorm.DB
	maxRetries := 5
	retryDelay := 500 * time.Millisecond
	connectTimeout := 10 * time.Second

	// Add connect_timeout to DSN if not present
	dsn := cfg.DataSource
	if !strings.Contains(dsn, "connect_timeout") {
		if strings.Contains(dsn, "?") {
			dsn = dsn + fmt.Sprintf("&connect_timeout=%d", int(connectTimeout.Seconds()))
		} else {
			dsn = dsn + fmt.Sprintf("?connect_timeout=%d", int(connectTimeout.Seconds()))
		}
	}

	// Parse the pgx connection config so we can inject a custom DNS resolver
	// when db.resolvers is configured.  This is especially useful in container
	// environments where the system DNS may only be reachable over IPv6 (e.g.
	// [::1]:53) but is not actually available, causing "connection refused"
	// errors during hostname resolution.
	pgxConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database DSN: %w", err)
	}
	if !cfg.PrepareStmt {
		pgxConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	if len(cfg.Resolvers) > 0 {
		pgxConfig.LookupFunc = makeResolverLookupFunc(cfg.Resolvers)
	}

	// Build the underlying *sql.DB from the pgx config.  The connection is
	// lazy – it is not established until the first ping (done by gorm.Open).
	sqlDB := stdlib.OpenDB(*pgxConfig)

	// Track whether the function succeeded so the deferred cleanup can decide
	// whether to close sqlDB.  When successful, the caller owns sqlDB through
	// the returned gorm.DB.
	succeeded := false
	defer func() {
		if !succeeded {
			sqlDB.Close()
		}
	}()

	for i := 0; i <= maxRetries; i++ {
		// Create a timeout context for this attempt so it can be cancelled
		attemptCtx, attemptCancel := context.WithTimeout(ctx, connectTimeout+5*time.Second)

		// Run gorm.Open in a goroutine so we can cancel it via context
		type result struct {
			db  *gorm.DB
			err error
		}
		resultCh := make(chan result, 1)

		go func() {
			db, err := gorm.Open(postgres.New(postgres.Config{
				Conn: sqlDB,
			}), &gorm.Config{
				Logger: NewLogger(lg, logCfg.SlowThreshold, logCfg.IgnoreRecordNotFound, level, logCfg),
				NamingStrategy: schema.NamingStrategy{
					TablePrefix:   "teldrive.",
					SingularTable: false,
				},
				NowFunc: func() time.Time {
					return time.Now().UTC()
				},
			})
			resultCh <- result{db: db, err: err}
		}()

		// Wait for either the result or context cancellation
		select {
		case <-attemptCtx.Done():
			attemptCancel()
			return nil, attemptCtx.Err()
		case res := <-resultCh:
			attemptCancel()
			db = res.db
			err = res.err
		}

		if err == nil {
			if i > 0 {
				lg.Info("db.connection.success", zap.Int("attempts", i+1))
			}
			break
		}

		if i < maxRetries {
			lg.Warn("db.connection.failed",
				zap.Int("attempt", i+1),
				zap.Int("max_retries", maxRetries),
				zap.Error(err),
				zap.Duration("retry_in", retryDelay))

			// Wait for retry delay but check context
			timer := time.NewTimer(retryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		} else {
			lg.Error("db.connection.failed_all_retries",
				zap.Int("max_retries", maxRetries),
				zap.Error(err))
			return nil, fmt.Errorf("database connection failed after %d attempts: %w", maxRetries, err)
		}
	}

	db.Use(extraClausePlugin.New())

	if cfg.Pool.Enable {
		rawDB, err := db.DB()
		if err != nil {
			return nil, err
		}
		rawDB.SetMaxOpenConns(cfg.Pool.MaxOpenConnections)
		rawDB.SetMaxIdleConns(cfg.Pool.MaxIdleConnections)
		rawDB.SetConnMaxLifetime(cfg.Pool.MaxLifetime)
	}

	succeeded = true
	return db, nil
}
