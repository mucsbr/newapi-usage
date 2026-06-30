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

// DefaultCPAUserAgent mirrors pool_maintainer.py's DEFAULT_MGMT_UA so the CPA
// management api-call probe presents the same client identity.
const DefaultCPAUserAgent = "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)"

type Config struct {
	Host                 string
	Port                 int
	SQLDSN               string
	DBDriver             Driver
	DBMaxOpenConns       int
	DBMaxIdleConns       int
	QueryTimeout         time.Duration
	ShowFullKeys         bool
	AdminPassword        string
	AuditLogGlob         string
	AuditIndexDSN        string
	AuditTimezone        string
	AuditScanInterval    time.Duration
	AuditLookupWindow    time.Duration
	AuditMaxLinesPerScan int

	// DeepSeek channel balance.
	DeepSeekAPIKey  string
	DeepSeekAPIBase string
	DeepSeekLabel   string

	// CPA channel pool balance.
	CPABaseURL              string
	CPAToken                string
	CPALabel                string
	CPATargetType           string
	CPAUserAgent            string
	CPAUsedPercentThreshold int
	CPAProbeConcurrency     int
	CPAProbeTimeout         time.Duration
	CPARefreshInterval      time.Duration
	CPAMaxAccounts          int
}

func Load() (Config, error) {
	cfg := Config{
		Host:                 getEnv("HOST", "0.0.0.0"),
		Port:                 getEnvInt("PORT", 8080),
		SQLDSN:               firstEnv("SQL_DSN", "NEWAPI_SQL_DSN", "DB_DSN"),
		DBMaxOpenConns:       getEnvInt("DB_MAX_OPEN_CONNS", 10),
		DBMaxIdleConns:       getEnvInt("DB_MAX_IDLE_CONNS", 5),
		QueryTimeout:         time.Duration(getEnvInt("QUERY_TIMEOUT_SECONDS", 30)) * time.Second,
		ShowFullKeys:         getEnvBool("SHOW_FULL_KEYS", false),
		AdminPassword:        getEnv("ADMIN_PASSWORD", ""),
		AuditLogGlob:         firstEnv("AUDIT_LOG_GLOB", "AUDIT_LOG_PATHS"),
		AuditIndexDSN:        getEnv("AUDIT_INDEX_DSN", "/var/lib/newapi-usage/audit.db"),
		AuditTimezone:        getEnv("AUDIT_TIMEZONE", "UTC"),
		AuditScanInterval:    time.Duration(getEnvInt("AUDIT_SCAN_INTERVAL_SECONDS", 10)) * time.Second,
		AuditLookupWindow:    time.Duration(getEnvInt("AUDIT_LOOKUP_WINDOW_SECONDS", 120)) * time.Second,
		AuditMaxLinesPerScan: getEnvInt("AUDIT_MAX_LINES_PER_SCAN", 50000),

		DeepSeekAPIKey:  firstEnv("DEEPSEEK_API_KEY", "DEEPSEEK_TOKEN"),
		DeepSeekAPIBase: getEnv("DEEPSEEK_API_BASE", "https://api.deepseek.com"),
		DeepSeekLabel:   getEnv("DEEPSEEK_LABEL", "DeepSeek"),

		CPABaseURL:              getEnv("CPA_BASE_URL", ""),
		CPAToken:                firstEnv("CPA_TOKEN", "CPA_MGMT_TOKEN"),
		CPALabel:                getEnv("CPA_LABEL", "CPA"),
		CPATargetType:           getEnv("CPA_TARGET_TYPE", "codex"),
		CPAUserAgent:            getEnv("CPA_USER_AGENT", DefaultCPAUserAgent),
		CPAUsedPercentThreshold: getEnvInt("CPA_USED_PERCENT_THRESHOLD", 95),
		CPAProbeConcurrency:     getEnvInt("CPA_PROBE_CONCURRENCY", 20),
		CPAProbeTimeout:         time.Duration(getEnvInt("CPA_PROBE_TIMEOUT_SECONDS", 15)) * time.Second,
		CPARefreshInterval:      time.Duration(getEnvInt("CPA_REFRESH_INTERVAL_SECONDS", 300)) * time.Second,
		CPAMaxAccounts:          getEnvInt("CPA_MAX_ACCOUNTS", 0),
	}
	if cfg.SQLDSN == "" {
		return Config{}, fmt.Errorf("SQL_DSN is required")
	}
	if cfg.AdminPassword == "" {
		return Config{}, fmt.Errorf("ADMIN_PASSWORD is required")
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
	if cfg.AuditScanInterval <= 0 {
		cfg.AuditScanInterval = 10 * time.Second
	}
	if cfg.AuditLookupWindow <= 0 {
		cfg.AuditLookupWindow = 120 * time.Second
	}
	if cfg.AuditMaxLinesPerScan <= 0 {
		cfg.AuditMaxLinesPerScan = 50000
	}

	cfg.DeepSeekAPIBase = strings.TrimRight(strings.TrimSpace(cfg.DeepSeekAPIBase), "/")
	if cfg.DeepSeekAPIBase == "" {
		cfg.DeepSeekAPIBase = "https://api.deepseek.com"
	}
	cfg.CPABaseURL = strings.TrimRight(strings.TrimSpace(cfg.CPABaseURL), "/")
	if strings.TrimSpace(cfg.CPATargetType) == "" {
		cfg.CPATargetType = "codex"
	}
	if strings.TrimSpace(cfg.CPAUserAgent) == "" {
		cfg.CPAUserAgent = DefaultCPAUserAgent
	}
	if cfg.CPAUsedPercentThreshold <= 0 {
		cfg.CPAUsedPercentThreshold = 95
	}
	if cfg.CPAProbeConcurrency <= 0 {
		cfg.CPAProbeConcurrency = 20
	}
	if cfg.CPAProbeTimeout <= 0 {
		cfg.CPAProbeTimeout = 15 * time.Second
	}
	if cfg.CPARefreshInterval <= 0 {
		cfg.CPARefreshInterval = 300 * time.Second
	}
	if cfg.CPAMaxAccounts < 0 {
		cfg.CPAMaxAccounts = 0
	}

	return cfg, nil
}

// DeepSeekEnabled reports whether a DeepSeek balance card should be served.
func (c Config) DeepSeekEnabled() bool {
	return strings.TrimSpace(c.DeepSeekAPIKey) != ""
}

// CPAEnabled reports whether a CPA pool balance card should be served.
func (c Config) CPAEnabled() bool {
	return strings.TrimSpace(c.CPABaseURL) != "" && strings.TrimSpace(c.CPAToken) != ""
}

// ChannelsEnabled reports whether the channel balance feature is active at all.
func (c Config) ChannelsEnabled() bool {
	return c.DeepSeekEnabled() || c.CPAEnabled()
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
