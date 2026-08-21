package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/mucsbr/newapi-usage/internal/config"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type Store struct {
	db           *sql.DB
	driver       config.Driver
	timeout      time.Duration
	showFullKeys bool
}

func Open(cfg config.Config) (*Store, error) {
	db, err := sql.Open(cfg.DriverName(), cfg.DriverDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.SetMaxIdleConns(cfg.DBMaxIdleConns)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(2 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.QueryTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{
		db:           db,
		driver:       cfg.DBDriver,
		timeout:      cfg.QueryTimeout,
		showFullKeys: cfg.ShowFullKeys,
	}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) Summary(ctx context.Context, tr TimeRange) (Summary, error) {
	return s.summary(ctx, tr, 0, true)
}

func (s *Store) TokenSummary(ctx context.Context, tr TimeRange, tokenID int64) (Summary, error) {
	if tokenID <= 0 {
		return Summary{}, fmt.Errorf("invalid token id")
	}
	return s.summary(ctx, tr, tokenID, false)
}

func (s *Store) summary(ctx context.Context, tr TimeRange, tokenID int64, includeCacheMetrics bool) (Summary, error) {
	ctx, cancel := s.context(ctx)
	defer cancel()

	first := ""
	args := make([]any, 0, 3)
	if tokenID > 0 {
		first = "l.token_id = " + s.placeholder(1)
		args = append(args, tokenID)
	}
	where, timeArgs := s.where("l", tr, first)
	args = append(args, timeArgs...)
	query := fmt.Sprintf(`
		SELECT
			COUNT(*) AS request_count,
			COALESCE(SUM(CASE WHEN l.type = 2 THEN 1 ELSE 0 END), 0) AS success_count,
			COALESCE(SUM(CASE WHEN l.type = 5 THEN 1 ELSE 0 END), 0) AS error_count,
			COUNT(DISTINCT l.token_id) AS token_count,
			COUNT(DISTINCT l.user_id) AS user_count,
			COUNT(DISTINCT NULLIF(l.model_name, '')) AS model_count,
			COALESCE(SUM(l.prompt_tokens), 0) AS input_tokens,
			COALESCE(SUM(l.completion_tokens), 0) AS output_tokens,
			COALESCE(SUM(l.prompt_tokens + l.completion_tokens), 0) AS total_tokens,
			COALESCE(SUM(l.quota), 0) AS quota,
			COALESCE(MIN(l.created_at), 0) AS first_used_at,
			COALESCE(MAX(l.created_at), 0) AS last_used_at
		FROM logs l
		%s`, where)

	var out Summary
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&out.RequestCount,
		&out.SuccessCount,
		&out.ErrorCount,
		&out.TokenCount,
		&out.UserCount,
		&out.ModelCount,
		&out.InputTokens,
		&out.OutputTokens,
		&out.TotalTokens,
		&out.Quota,
		&out.FirstUsedAt,
		&out.LastUsedAt,
	)
	if err != nil {
		return Summary{}, err
	}
	if !includeCacheMetrics {
		out.QuotaCNY = quotaToCNY(float64(out.Quota), s.billingSettings(ctx))
		out.GeneratedAt = time.Now().Unix()
		return out, nil
	}

	cacheArgs := append([]any{}, args...)
	cacheArgs = append(cacheArgs, `%"cache_tokens"%`)
	cacheQuery := fmt.Sprintf(`SELECT COALESCE(l.prompt_tokens, 0), COALESCE(l.other, '') FROM logs l %s AND l.other LIKE %s`,
		where, s.placeholder(len(cacheArgs)))
	rows, err := s.db.QueryContext(ctx, cacheQuery, cacheArgs...)
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()
	cacheRateInputTokens := out.InputTokens
	for rows.Next() {
		var promptTokens int64
		var other string
		if err := rows.Scan(&promptTokens, &other); err != nil {
			return Summary{}, err
		}
		meta := parseLogBillingMeta(other)
		out.CacheReadTokens += meta.cacheReadTokens
		if meta.inputTokensTotal > promptTokens {
			cacheRateInputTokens += meta.inputTokensTotal - promptTokens
		} else if meta.anthropicUsageSemantic {
			cacheRateInputTokens += meta.cacheReadTokens + meta.cacheWriteTokens
		}
	}
	if err := rows.Err(); err != nil {
		return Summary{}, err
	}
	if err := rows.Close(); err != nil {
		return Summary{}, err
	}
	out.InputTokens = cacheRateInputTokens
	out.TotalTokens = out.InputTokens + out.OutputTokens
	if cacheRateInputTokens > 0 {
		out.CacheRate = float64(out.CacheReadTokens) / float64(cacheRateInputTokens) * 100
		if out.CacheRate < 0 {
			out.CacheRate = 0
		} else if out.CacheRate > 100 {
			out.CacheRate = 100
		}
	}
	out.QuotaCNY = quotaToCNY(float64(out.Quota), s.billingSettings(ctx))
	out.GeneratedAt = time.Now().Unix()
	return out, nil
}

func (s *Store) KeyUsage(ctx context.Context, filter KeyFilter) ([]KeyUsage, error) {
	ctx, cancel := s.context(ctx)
	defer cancel()

	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	where, args := s.where("l", filter.TimeRange, "")
	if strings.TrimSpace(filter.Query) != "" {
		pattern := "%" + strings.ToLower(strings.TrimSpace(filter.Query)) + "%"
		where += fmt.Sprintf(` AND (
			LOWER(COALESCE(t.name, '')) LIKE %s OR
			LOWER(COALESCE(l.token_name, '')) LIKE %s OR
			LOWER(COALESCE(l.username, '')) LIKE %s OR
			LOWER(COALESCE(l.model_name, '')) LIKE %s
		)`, s.placeholder(len(args)+1), s.placeholder(len(args)+2), s.placeholder(len(args)+3), s.placeholder(len(args)+4))
		args = append(args, pattern, pattern, pattern, pattern)
	}
	args = append(args, filter.Limit)

	keyValueSelect := "'' AS key_value,"
	if s.showFullKeys {
		keyValueSelect = fmt.Sprintf("COALESCE(MAX(%s), '') AS key_value,", s.tokenKeyColumn("t"))
	}
	orderBy := "total_tokens DESC, request_count DESC"
	switch strings.ToLower(strings.TrimSpace(filter.Sort)) {
	case "requests", "request_count", "calls":
		orderBy = "request_count DESC, total_tokens DESC"
	case "cost", "quota", "money":
		orderBy = "quota DESC, request_count DESC"
	}
	query := fmt.Sprintf(`
		SELECT
			COALESCE(l.token_id, 0) AS token_id,
			COALESCE(NULLIF(MAX(t.name), ''), NULLIF(MAX(l.token_name), ''), '') AS key_name,
			COALESCE(MAX(%s), '') AS key_tail,
			%s
			COALESCE(MAX(l.user_id), 0) AS user_id,
			COALESCE(MAX(l.username), '') AS username,
			COUNT(*) AS request_count,
			COALESCE(SUM(CASE WHEN l.type = 2 THEN 1 ELSE 0 END), 0) AS success_count,
			COALESCE(SUM(CASE WHEN l.type = 5 THEN 1 ELSE 0 END), 0) AS error_count,
			COUNT(DISTINCT NULLIF(l.model_name, '')) AS model_count,
			COALESCE(SUM(l.prompt_tokens), 0) AS input_tokens,
			COALESCE(SUM(l.completion_tokens), 0) AS output_tokens,
			COALESCE(SUM(l.prompt_tokens + l.completion_tokens), 0) AS total_tokens,
			COALESCE(SUM(l.quota), 0) AS quota,
			COALESCE(MIN(l.created_at), 0) AS first_used_at,
			COALESCE(MAX(l.created_at), 0) AS last_used_at
		FROM logs l
		LEFT JOIN tokens t ON t.id = l.token_id
		%s
		GROUP BY l.token_id
		ORDER BY %s
		LIMIT %s`, s.keyTailExpr("t"), keyValueSelect, where, orderBy, s.placeholder(len(args)))

	settings := s.billingSettings(ctx)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]KeyUsage, 0)
	for rows.Next() {
		var item KeyUsage
		if err := rows.Scan(
			&item.TokenID,
			&item.KeyName,
			&item.KeyTail,
			&item.KeyValue,
			&item.UserID,
			&item.Username,
			&item.RequestCount,
			&item.SuccessCount,
			&item.ErrorCount,
			&item.ModelCount,
			&item.InputTokens,
			&item.OutputTokens,
			&item.TotalTokens,
			&item.Quota,
			&item.FirstUsedAt,
			&item.LastUsedAt,
		); err != nil {
			return nil, err
		}
		item.QuotaCNY = quotaToCNY(float64(item.Quota), settings)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ModelUsage(ctx context.Context, filter ModelFilter) ([]ModelUsage, error) {
	return s.modelUsageWithBilling(ctx, filter)
}

func (s *Store) Logs(ctx context.Context, filter LogFilter) (LogPage, error) {
	ctx, cancel := s.context(ctx)
	defer cancel()

	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 || filter.PageSize > 500 {
		filter.PageSize = 100
	}
	conditions := []string{"l.type IN (2, 5)"}
	args := make([]any, 0)
	if filter.TokenID > 0 {
		args = append(args, filter.TokenID)
		conditions = append(conditions, "l.token_id = "+s.placeholder(len(args)))
	}
	if filter.Start > 0 {
		args = append(args, filter.Start)
		conditions = append(conditions, "l.created_at >= "+s.placeholder(len(args)))
	}
	if filter.End > 0 {
		args = append(args, filter.End)
		conditions = append(conditions, "l.created_at <= "+s.placeholder(len(args)))
	}
	if strings.TrimSpace(filter.Model) != "" {
		args = append(args, strings.TrimSpace(filter.Model))
		conditions = append(conditions, "l.model_name = "+s.placeholder(len(args)))
	}
	if strings.TrimSpace(filter.KeyName) != "" {
		pattern := "%" + strings.ToLower(strings.TrimSpace(filter.KeyName)) + "%"
		args = append(args, pattern, pattern)
		conditions = append(conditions, fmt.Sprintf(`(
			LOWER(COALESCE(t.name, '')) LIKE %s OR
			LOWER(COALESCE(l.token_name, '')) LIKE %s
		)`, s.placeholder(len(args)-1), s.placeholder(len(args))))
	}
	switch strings.ToLower(strings.TrimSpace(filter.LogType)) {
	case "success":
		conditions = append(conditions, "l.type = 2")
	case "error":
		conditions = append(conditions, "l.type = 5")
	}
	if strings.TrimSpace(filter.Query) != "" {
		pattern := "%" + strings.ToLower(strings.TrimSpace(filter.Query)) + "%"
		args = append(args, pattern, pattern, pattern, pattern)
		conditions = append(conditions, fmt.Sprintf(`(
			LOWER(COALESCE(l.username, '')) LIKE %s OR
			LOWER(COALESCE(l.token_name, '')) LIKE %s OR
			LOWER(COALESCE(l.model_name, '')) LIKE %s OR
			LOWER(COALESCE(l.request_id, '')) LIKE %s
		)`, s.placeholder(len(args)-3), s.placeholder(len(args)-2), s.placeholder(len(args)-1), s.placeholder(len(args))))
	}
	where := "WHERE " + strings.Join(conditions, " AND ")

	countQuery := "SELECT COUNT(*) FROM logs l LEFT JOIN tokens t ON t.id = l.token_id " + where
	var total int64
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return LogPage{}, err
	}

	offset := (filter.Page - 1) * filter.PageSize
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, filter.PageSize, offset)
	query := fmt.Sprintf(`
		SELECT
			l.id,
			COALESCE(l.created_at, 0),
			COALESCE(l.type, 0),
			COALESCE(l.request_id, ''),
			COALESCE(l.user_id, 0),
			COALESCE(l.username, ''),
			COALESCE(l.token_id, 0),
			COALESCE(l.token_name, ''),
			COALESCE(NULLIF(t.name, ''), NULLIF(l.token_name, ''), ''),
			COALESCE(%s, ''),
			COALESCE(l.model_name, ''),
			COALESCE(l.prompt_tokens, 0),
			COALESCE(l.completion_tokens, 0),
			COALESCE(l.prompt_tokens + l.completion_tokens, 0),
			COALESCE(l.quota, 0),
			COALESCE(l.use_time, 0),
			COALESCE(l.is_stream, false),
			COALESCE(l.channel_id, 0),
			COALESCE(l.channel_name, ''),
			COALESCE(l.ip, ''),
			COALESCE(l.content, ''),
			COALESCE(l.other, '')
		FROM logs l
		LEFT JOIN tokens t ON t.id = l.token_id
		%s
		ORDER BY l.id DESC
		LIMIT %s OFFSET %s`, s.keyTailExpr("t"), where, s.placeholder(len(queryArgs)-1), s.placeholder(len(queryArgs)))

	settings := s.billingSettings(ctx)
	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return LogPage{}, err
	}
	defer rows.Close()

	items := make([]UsageLog, 0)
	for rows.Next() {
		var item UsageLog
		if err := rows.Scan(
			&item.ID,
			&item.CreatedAt,
			&item.Type,
			&item.RequestID,
			&item.UserID,
			&item.Username,
			&item.TokenID,
			&item.TokenName,
			&item.KeyName,
			&item.KeyTail,
			&item.ModelName,
			&item.InputTokens,
			&item.OutputTokens,
			&item.TotalTokens,
			&item.Quota,
			&item.UseTime,
			&item.IsStream,
			&item.ChannelID,
			&item.ChannelName,
			&item.IP,
			&item.Content,
			&item.Other,
		); err != nil {
			return LogPage{}, err
		}
		s.enrichUsageLog(&item, settings)
		items = append(items, item)
	}

	return LogPage{Items: items, Total: total, Page: filter.Page, PageSize: filter.PageSize}, rows.Err()
}

func (s *Store) LogByID(ctx context.Context, id int64) (UsageLog, error) {
	ctx, cancel := s.context(ctx)
	defer cancel()

	query := fmt.Sprintf(`
		SELECT
			l.id,
			COALESCE(l.created_at, 0),
			COALESCE(l.type, 0),
			COALESCE(l.request_id, ''),
			COALESCE(l.user_id, 0),
			COALESCE(l.username, ''),
			COALESCE(l.token_id, 0),
			COALESCE(l.token_name, ''),
			COALESCE(NULLIF(t.name, ''), NULLIF(l.token_name, ''), ''),
			COALESCE(%s, ''),
			COALESCE(l.model_name, ''),
			COALESCE(l.prompt_tokens, 0),
			COALESCE(l.completion_tokens, 0),
			COALESCE(l.prompt_tokens + l.completion_tokens, 0),
			COALESCE(l.quota, 0),
			COALESCE(l.use_time, 0),
			COALESCE(l.is_stream, false),
			COALESCE(l.channel_id, 0),
			COALESCE(l.channel_name, ''),
			COALESCE(l.ip, ''),
			COALESCE(l.content, ''),
			COALESCE(l.other, '')
		FROM logs l
		LEFT JOIN tokens t ON t.id = l.token_id
		WHERE l.id = %s
		LIMIT 1`, s.keyTailExpr("t"), s.placeholder(1))

	var item UsageLog
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&item.ID,
		&item.CreatedAt,
		&item.Type,
		&item.RequestID,
		&item.UserID,
		&item.Username,
		&item.TokenID,
		&item.TokenName,
		&item.KeyName,
		&item.KeyTail,
		&item.ModelName,
		&item.InputTokens,
		&item.OutputTokens,
		&item.TotalTokens,
		&item.Quota,
		&item.UseTime,
		&item.IsStream,
		&item.ChannelID,
		&item.ChannelName,
		&item.IP,
		&item.Content,
		&item.Other,
	)
	if err == nil {
		s.enrichUsageLog(&item, s.billingSettings(ctx))
	}
	return item, err
}

func (s *Store) ResolveTokenByKey(key string) (TokenIdentity, error) {
	ctx, cancel := s.context(context.Background())
	defer cancel()

	query := fmt.Sprintf(`SELECT id, COALESCE(name, ''), COALESCE(%s, '') FROM tokens WHERE %s = %s LIMIT 1`, s.keyTailExpr("tokens"), s.tokenKeyColumn("tokens"), s.placeholder(1))
	var lastErr error
	for _, candidate := range tokenKeyCandidates(key) {
		var out TokenIdentity
		err := s.db.QueryRowContext(ctx, query, candidate).Scan(&out.TokenID, &out.Name, &out.KeyTail)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if err != sql.ErrNoRows {
			return TokenIdentity{}, err
		}
	}
	if lastErr != nil {
		return TokenIdentity{}, lastErr
	}
	return TokenIdentity{}, sql.ErrNoRows
}

func (s *Store) TokenOptions(ctx context.Context, limit int) ([]TokenOption, error) {
	ctx, cancel := s.context(ctx)
	defer cancel()
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	query := fmt.Sprintf(`SELECT id, COALESCE(name, ''), COALESCE(%s, '')
		FROM tokens ORDER BY CASE WHEN COALESCE(name, '') = '' THEN 1 ELSE 0 END, name, id LIMIT %s`,
		s.keyTailExpr("tokens"), s.placeholder(1))
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TokenOption, 0)
	for rows.Next() {
		var item TokenOption
		if err := rows.Scan(&item.TokenID, &item.Name, &item.KeyTail); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) context(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, s.timeout)
}

func (s *Store) where(alias string, tr TimeRange, first string) (string, []any) {
	conditions := []string{"(" + alias + ".type = 2 OR " + alias + ".type = 5)"}
	args := make([]any, 0)
	if first != "" {
		conditions = append([]string{first}, conditions...)
	}
	if tr.Start > 0 {
		args = append(args, tr.Start)
		conditions = append(conditions, alias+".created_at >= "+s.placeholder(len(args)+s.initialArgOffset(first)))
	}
	if tr.End > 0 {
		args = append(args, tr.End)
		conditions = append(conditions, alias+".created_at <= "+s.placeholder(len(args)+s.initialArgOffset(first)))
	}
	return "WHERE " + strings.Join(conditions, " AND "), args
}

func (s *Store) initialArgOffset(first string) int {
	if first == "" {
		return 0
	}
	return 1
}

func (s *Store) placeholder(index int) string {
	if s.driver == config.DriverPostgres {
		return fmt.Sprintf("$%d", index)
	}
	return "?"
}

func (s *Store) tokenKeyColumn(alias string) string {
	switch s.driver {
	case config.DriverPostgres:
		return alias + `."key"`
	case config.DriverMySQL:
		return alias + ".`key`"
	default:
		return alias + ".key"
	}
}

func (s *Store) keyTailExpr(alias string) string {
	keyCol := s.tokenKeyColumn(alias)
	switch s.driver {
	case config.DriverSQLite:
		return "substr(" + keyCol + ", -8)"
	default:
		return "RIGHT(" + keyCol + ", 8)"
	}
}

func tokenKeyCandidates(key string) []string {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(strings.ToLower(key), "bearer ") {
		key = strings.TrimSpace(key[7:])
	}
	candidates := make([]string, 0, 2)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range candidates {
			if existing == value {
				return
			}
		}
		candidates = append(candidates, value)
	}
	add(key)
	if strings.HasPrefix(strings.ToLower(key), "sk-") {
		add(key[3:])
	}
	return candidates
}
