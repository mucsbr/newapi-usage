package store

type TimeRange struct {
	Start int64
	End   int64
}

type KeyFilter struct {
	TimeRange
	Query string
	Limit int
}

type ModelFilter struct {
	TimeRange
	TokenID int64
	Limit   int
}

type LogFilter struct {
	TimeRange
	TokenID  int64
	Model    string
	Query    string
	LogType  string
	Page     int
	PageSize int
}

type Summary struct {
	RequestCount int64 `json:"request_count"`
	SuccessCount int64 `json:"success_count"`
	ErrorCount   int64 `json:"error_count"`
	TokenCount   int64 `json:"token_count"`
	UserCount    int64 `json:"user_count"`
	ModelCount   int64 `json:"model_count"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	Quota        int64 `json:"quota"`
	FirstUsedAt  int64 `json:"first_used_at"`
	LastUsedAt   int64 `json:"last_used_at"`
	GeneratedAt  int64 `json:"generated_at"`
}

type KeyUsage struct {
	TokenID      int64  `json:"token_id"`
	KeyName      string `json:"key_name"`
	KeyTail      string `json:"key_tail"`
	KeyValue     string `json:"key_value,omitempty"`
	UserID       int64  `json:"user_id"`
	Username     string `json:"username"`
	RequestCount int64  `json:"request_count"`
	SuccessCount int64  `json:"success_count"`
	ErrorCount   int64  `json:"error_count"`
	ModelCount   int64  `json:"model_count"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	Quota        int64  `json:"quota"`
	FirstUsedAt  int64  `json:"first_used_at"`
	LastUsedAt   int64  `json:"last_used_at"`
}

type ModelUsage struct {
	ModelName    string `json:"model_name"`
	RequestCount int64  `json:"request_count"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	Quota        int64  `json:"quota"`
	SuccessCount int64  `json:"success_count"`
	ErrorCount   int64  `json:"error_count"`
	FirstUsedAt  int64  `json:"first_used_at"`
	LastUsedAt   int64  `json:"last_used_at"`
}

type UsageLog struct {
	ID           int64  `json:"id"`
	CreatedAt    int64  `json:"created_at"`
	Type         int64  `json:"type"`
	RequestID    string `json:"request_id"`
	UserID       int64  `json:"user_id"`
	Username     string `json:"username"`
	TokenID      int64  `json:"token_id"`
	TokenName    string `json:"token_name"`
	KeyName      string `json:"key_name"`
	KeyTail      string `json:"key_tail"`
	ModelName    string `json:"model_name"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	Quota        int64  `json:"quota"`
	UseTime      int64  `json:"use_time"`
	IsStream     bool   `json:"is_stream"`
	ChannelID    int64  `json:"channel_id"`
	ChannelName  string `json:"channel_name"`
	IP           string `json:"ip"`
	Content      string `json:"content"`
	Other        string `json:"other"`
}

type LogPage struct {
	Items    []UsageLog `json:"items"`
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
}

type TokenIdentity struct {
	TokenID int64
	Name    string
	KeyTail string
}
