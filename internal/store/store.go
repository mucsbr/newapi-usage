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
	ctx, cancel := s.context(ctx)
	defer cancel()

	where, args := s.where("l", tr, "")
	query := fmt.Sprintf(`
		SELECT
			COUNT(*) AS request_count,
			SUM(CASE WHEN l.type = 2 THEN 1 ELSE 0 END) AS success_count,
			SUM(CASE WHEN l.type = 5 THEN 1 ELSE 0 END) AS error_count,
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
	out.GeneratedAt = time.Now().Unix()
	return out, err
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
	query := fmt.Sprintf(`
		SELECT
			COALESCE(l.token_id, 0) AS token_id,
			COALESCE(NULLIF(MAX(t.name), ''), NULLIF(MAX(l.token_name), ''), '') AS key_name,
			COALESCE(MAX(%s), '') AS key_tail,
			%s
			COALESCE(MAX(l.user_id), 0) AS user_id,
			COALESCE(MAX(l.username), '') AS username,
			COUNT(*) AS request_count,
			SUM(CASE WHEN l.type = 2 THEN 1 ELSE 0 END) AS success_count,
			SUM(CASE WHEN l.type = 5 THEN 1 ELSE 0 END) AS error_count,
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
		ORDER BY total_tokens DESC, request_count DESC
		LIMIT %s`, s.keyTailExpr("t"), keyValueSelect, where, s.placeholder(len(args)))

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
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ModelUsage(ctx context.Context, filter ModelFilter) ([]ModelUsage, error) {
	ctx, cancel := s.context(ctx)
	defer cancel()

	if filter.TokenID <= 0 {
		return []ModelUsage{}, nil
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	where, args := s.where("l", filter.TimeRange, "l.token_id = "+s.placeholder(1))
	args = append([]any{filter.TokenID}, args...)
	args = append(args, filter.Limit)

	query := fmt.Sprintf(`
		SELECT
			COALESCE(NULLIF(l.model_name, ''), 'unknown') AS model_name,
			COUNT(*) AS request_count,
			COALESCE(SUM(l.prompt_tokens), 0) AS input_tokens,
			COALESCE(SUM(l.completion_tokens), 0) AS output_tokens,
			COALESCE(SUM(l.prompt_tokens + l.completion_tokens), 0) AS total_tokens,
			COALESCE(SUM(l.quota), 0) AS quota,
			SUM(CASE WHEN l.type = 2 THEN 1 ELSE 0 END) AS success_count,
			SUM(CASE WHEN l.type = 5 THEN 1 ELSE 0 END) AS error_count,
			COALESCE(MIN(l.created_at), 0) AS first_used_at,
			COALESCE(MAX(l.created_at), 0) AS last_used_at
		FROM logs l
		%s
		GROUP BY COALESCE(NULLIF(l.model_name, ''), 'unknown')
		ORDER BY total_tokens DESC, request_count DESC
		LIMIT %s`, where, s.placeholder(len(args)))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]ModelUsage, 0)
	for rows.Next() {
		var item ModelUsage
		if err := rows.Scan(
			&item.ModelName,
			&item.RequestCount,
			&item.InputTokens,
			&item.OutputTokens,
			&item.TotalTokens,
			&item.Quota,
			&item.SuccessCount,
			&item.ErrorCount,
			&item.FirstUsedAt,
			&item.LastUsedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
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

	countQuery := "SELECT COUNT(*) FROM logs l " + where
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
