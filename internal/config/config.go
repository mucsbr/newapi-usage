package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Driver string

const (
	DriverPostgres Driver = "postgres"
	DriverMySQL    Driver = "mysql"
	DriverSQLite   Driver = "sqlite"
)

type Config struct {
	Host           string
	Port           int
	SQLDSN         string
	DBDriver       Driver
	DBMaxOpenConns int
	DBMaxIdleConns int
	QueryTimeout   time.Duration
	ShowFullKeys   bool
}

func Load() (Config, error) {
	cfg := Config{
		Host:           getEnv("HOST", "0.0.0.0"),
		Port:           getEnvInt("PORT", 8080),
		SQLDSN:         firstEnv("SQL_DSN", "NEWAPI_SQL_DSN", "DB_DSN"),
		DBMaxOpenConns: getEnvInt("DB_MAX_OPEN_CONNS", 10),
		DBMaxIdleConns: getEnvInt("DB_MAX_IDLE_CONNS", 5),
		QueryTimeout:   time.Duration(getEnvInt("QUERY_TIMEOUT_SECONDS", 30)) * time.Second,
		ShowFullKeys:   getEnvBool("SHOW_FULL_KEYS", false),
	}
	if cfg.SQLDSN == "" {
		return Config{}, fmt.Errorf("SQL_DSN is required")
	}

	driver := strings.ToLower(strings.TrimSpace(firstEnv("DB_DRIVER", "DB_ENGINE")))
	if driver == "" {
		driver = string(detectDriver(cfg.SQLDSN))
	}
	switch driver {
	case "postgres", "postgresql", "pg":
		cfg.DBDriver = DriverPostgres
	case "mysql", "mariadb":
		cfg.DBDriver = DriverMySQL
	case "sqlite", "sqlite3":
		cfg.DBDriver = DriverSQLite
	default:
		return Config{}, fmt.Errorf("unsupported DB_DRIVER %q", driver)
	}

	if cfg.DBMaxOpenConns <= 0 {
		cfg.DBMaxOpenConns = 10
	}
	if cfg.DBMaxIdleConns <= 0 {
		cfg.DBMaxIdleConns = 5
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 30 * time.Second
	}

	return cfg, nil
}

func (c Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

func (c Config) DriverName() string {
	switch c.DBDriver {
	case DriverPostgres:
		return "pgx"
	case DriverSQLite:
		return "sqlite"
	default:
		return "mysql"
	}
}

func (c Config) DriverDSN() string {
	if c.DBDriver == DriverMySQL {
		return strings.TrimPrefix(c.SQLDSN, "mysql://")
	}
	if c.DBDriver == DriverSQLite {
		return strings.TrimPrefix(strings.TrimPrefix(c.SQLDSN, "sqlite://"), "sqlite3://")
	}
	return c.SQLDSN
}

func detectDriver(dsn string) Driver {
	lower := strings.ToLower(strings.TrimSpace(dsn))
	switch {
	case strings.HasPrefix(lower, "postgres://"),
		strings.HasPrefix(lower, "postgresql://"),
		strings.Contains(lower, "host="):
		return DriverPostgres
	case strings.HasPrefix(lower, "mysql://"),
		strings.Contains(lower, "@tcp("):
		return DriverMySQL
	case strings.HasPrefix(lower, "sqlite://"),
		strings.HasPrefix(lower, "sqlite3://"),
		strings.HasPrefix(lower, "file:"),
		strings.HasSuffix(lower, ".db"),
		strings.HasSuffix(lower, ".sqlite"),
		strings.HasSuffix(lower, ".sqlite3"):
		return DriverSQLite
	default:
		return DriverMySQL
	}
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func getEnv(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getEnvBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}
