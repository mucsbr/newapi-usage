package review

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotConfigured = errors.New("review model is not configured")
	ErrNotFound      = errors.New("review job not found")
)

const (
	concurrentJobWorkers     = 3
	globalRequestConcurrency = 10
)

type Manager struct {
	db           *sql.DB
	cfg          Config
	secret       []byte
	client       *http.Client
	wake         chan struct{}
	requestSlots chan struct{}
	wg           sync.WaitGroup
	closeOnce    sync.Once
}

func Open(cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.IndexDSN) == "" {
		return nil, fmt.Errorf("review index dsn is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 2 * time.Second
	}
	db, err := sql.Open("sqlite", cfg.IndexDSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	secret, err := loadOrCreateSecret(cfg)
	if err != nil {
		db.Close()
		return nil, err
	}
	m := &Manager{
		db:           db,
		cfg:          cfg,
		secret:       secret,
		client:       &http.Client{Timeout: cfg.Timeout},
		wake:         make(chan struct{}, 1),
		requestSlots: make(chan struct{}, globalRequestConcurrency),
	}
	if err := m.initSchema(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`UPDATE review_jobs SET status = 'queued', updated_at = ? WHERE status IN ('claimed', 'planning', 'running')`, time.Now().Unix()); err != nil {
		db.Close()
		return nil, err
	}
	return m, nil
}

func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	for range concurrentJobWorkers {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.run(ctx)
		}()
	}
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.wg.Wait()
	var err error
	m.closeOnce.Do(func() { err = m.db.Close() })
	return err
}

func (m *Manager) run(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.PollEvery)
	defer ticker.Stop()
	for {
		if err := m.runNextJob(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("review job processing failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wake:
		}
	}
}

func (m *Manager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) encrypt(value string) ([]byte, error) {
	block, err := aes.NewCipher(m.secret)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(value), nil), nil
}

func (m *Manager) decrypt(value []byte) (string, error) {
	block, err := aes.NewCipher(m.secret)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(value) < gcm.NonceSize() {
		return "", fmt.Errorf("invalid encrypted api key")
	}
	plain, err := gcm.Open(nil, value[:gcm.NonceSize()], value[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func loadOrCreateSecret(cfg Config) ([]byte, error) {
	if cfg.IndexDSN == ":memory:" || strings.HasPrefix(cfg.IndexDSN, "file::memory:") {
		secret := make([]byte, 32)
		_, err := rand.Read(secret)
		return secret, err
	}
	path := strings.TrimSpace(cfg.SecretPath)
	if path == "" {
		path = indexFilePath(cfg.IndexDSN) + ".review.key"
	}
	if data, err := os.ReadFile(path); err == nil {
		if len(data) != 32 {
			return nil, fmt.Errorf("invalid review secret length")
		}
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		if len(data) != 32 {
			return nil, fmt.Errorf("invalid review secret length")
		}
		return data, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(secret); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return secret, nil
}

func indexFilePath(dsn string) string {
	path := strings.TrimSpace(dsn)
	if strings.HasPrefix(path, "file:") {
		path = strings.TrimPrefix(path, "file:")
		if idx := strings.Index(path, "?"); idx >= 0 {
			path = path[:idx]
		}
	}
	return path
}
