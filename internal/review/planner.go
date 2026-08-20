package review

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mucsbr/newapi-usage/internal/audit"
)

const (
	planBatchSize    = 200
	branchHistory    = 100
	anchorEntryLimit = 50
)

type auditRequest struct {
	ID        int64
	TokenID   int64
	CreatedAt int64
	Model     string
	Messages  []audit.Message
}

type conversationBranch struct {
	AuditEntryID int64
	Hashes       []string
}

type plannedEntry struct {
	AuditEntryID      int64
	TokenID           int64
	CreatedAt         int64
	Model             string
	ParentEntryID     int64
	DeltaStart        int
	DeltaMessageCount int
	ContentHash       string
	ContentChars      int
}

func (m *Manager) planJob(ctx context.Context, job Job) error {
	if _, err := m.db.ExecContext(ctx, `DELETE FROM review_job_entries WHERE job_id = ?`, job.ID); err != nil {
		return err
	}
	var total, units, chars int64
	for _, tokenID := range job.TokenIDs {
		if err := m.ensureJobActive(ctx, job.ID, StatusPlanning); err != nil {
			return err
		}
		branches, err := m.loadAnchorBranches(ctx, tokenID, job.Start, job.MaxEntryID)
		if err != nil {
			return err
		}
		cursorTime, cursorID := int64(0), int64(0)
		for {
			batch, err := m.loadAuditBatch(ctx, tokenID, job.Start, job.End, job.MaxEntryID, cursorTime, cursorID)
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			planned := make([]plannedEntry, 0, len(batch))
			for _, request := range batch {
				entry := planRequest(request, branches, job.RoleMode)
				planned = append(planned, entry)
				total++
				if entry.ContentHash != "" {
					units++
					chars += int64(entry.ContentChars)
				}
				branches = appendBranch(branches, conversationBranch{AuditEntryID: request.ID, Hashes: messageHashes(request.Messages)})
			}
			if err := m.insertPlannedEntries(ctx, job.ID, planned); err != nil {
				return err
			}
			last := batch[len(batch)-1]
			cursorTime, cursorID = last.CreatedAt, last.ID
			if err := m.ensureJobActive(ctx, job.ID, StatusPlanning); err != nil {
				return err
			}
		}
	}
	_, err := m.db.ExecContext(ctx, `UPDATE review_jobs SET
		total_entries = ?, review_units = ?, estimated_chars = ?, processed_entries = 0,
		reviewed_units = 0, cache_hits = 0, flagged_entries = 0, error_entries = 0,
		prompt_tokens = 0, completion_tokens = 0, updated_at = ? WHERE id = ?`,
		total, units, chars, time.Now().Unix(), job.ID)
	if err == nil {
		slog.Info("review job planning completed", "job_id", job.ID, "entries", total, "review_units", units, "estimated_chars", chars)
	}
	return err
}

func (m *Manager) loadAnchorBranches(ctx context.Context, tokenID, start, maxEntryID int64) ([]conversationBranch, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT id, token_id, created_at, model, body, body_gzip, body_encoding
		FROM audit_entries WHERE token_id = ? AND created_at < ? AND id <= ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, tokenID, start, maxEntryID, anchorEntryLimit)
	if err != nil {
		return nil, err
	}
	items, err := scanAuditRequests(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(a, b int) bool {
		if items[a].CreatedAt == items[b].CreatedAt {
			return items[a].ID < items[b].ID
		}
		return items[a].CreatedAt < items[b].CreatedAt
	})
	branches := make([]conversationBranch, 0, len(items))
	for _, item := range items {
		branches = appendBranch(branches, conversationBranch{AuditEntryID: item.ID, Hashes: messageHashes(item.Messages)})
	}
	return branches, nil
}

func (m *Manager) loadAuditBatch(ctx context.Context, tokenID, start, end, maxEntryID, cursorTime, cursorID int64) ([]auditRequest, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT id, token_id, created_at, model, body, body_gzip, body_encoding
		FROM audit_entries
		WHERE token_id = ? AND created_at >= ? AND created_at <= ? AND id <= ?
		AND (created_at > ? OR (created_at = ? AND id > ?))
		ORDER BY created_at, id LIMIT ?`,
		tokenID, start, end, maxEntryID, cursorTime, cursorTime, cursorID, planBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAuditRequests(rows)
}

func scanAuditRequests(rows *sql.Rows) ([]auditRequest, error) {
	items := make([]auditRequest, 0)
	for rows.Next() {
		var item auditRequest
		var body string
		var bodyGzip []byte
		var bodyEncoding string
		if err := rows.Scan(&item.ID, &item.TokenID, &item.CreatedAt, &item.Model, &body, &bodyGzip, &bodyEncoding); err != nil {
			return nil, err
		}
		decoded, err := audit.DecodeStoredBody(body, bodyGzip, bodyEncoding)
		if err != nil {
			return nil, err
		}
		item.Messages = audit.NormalizeMessages(decoded)
		items = append(items, item)
	}
	return items, rows.Err()
}

func planRequest(request auditRequest, branches []conversationBranch, roleMode string) plannedEntry {
	hashes := messageHashes(request.Messages)
	parent, deltaStart := findParentBranch(hashes, branches)
	if parent == 0 {
		deltaStart = lastReviewableIndex(request.Messages, roleMode)
	}
	content, count := deltaContent(request.Messages, deltaStart, roleMode)
	hash := ""
	if content != "" {
		sum := sha256.Sum256([]byte(content))
		hash = hex.EncodeToString(sum[:])
	}
	return plannedEntry{
		AuditEntryID:      request.ID,
		TokenID:           request.TokenID,
		CreatedAt:         request.CreatedAt,
		Model:             request.Model,
		ParentEntryID:     parent,
		DeltaStart:        deltaStart,
		DeltaMessageCount: count,
		ContentHash:       hash,
		ContentChars:      len([]rune(content)),
	}
}

func findParentBranch(current []string, branches []conversationBranch) (int64, int) {
	var parentID int64
	best := 0
	for _, branch := range branches {
		if len(branch.Hashes) == 0 || len(branch.Hashes) > len(current) || len(branch.Hashes) <= best {
			continue
		}
		matched := true
		for index := range branch.Hashes {
			if branch.Hashes[index] != current[index] {
				matched = false
				break
			}
		}
		if matched {
			parentID = branch.AuditEntryID
			best = len(branch.Hashes)
		}
	}
	return parentID, best
}

func appendBranch(branches []conversationBranch, branch conversationBranch) []conversationBranch {
	if len(branch.Hashes) == 0 {
		return branches
	}
	branches = append(branches, branch)
	if len(branches) > branchHistory {
		branches = branches[len(branches)-branchHistory:]
	}
	return branches
}

func messageHashes(messages []audit.Message) []string {
	hashes := make([]string, 0, len(messages))
	for _, message := range messages {
		value := strings.ToLower(strings.TrimSpace(message.Role)) + "\x00" + normalizeContent(message.Content)
		sum := sha256.Sum256([]byte(value))
		hashes = append(hashes, hex.EncodeToString(sum[:]))
	}
	return hashes
}

func lastReviewableIndex(messages []audit.Message, roleMode string) int {
	for index := len(messages) - 1; index >= 0; index-- {
		if roleAllowed(messages[index].Role, roleMode) && normalizeContent(messages[index].Content) != "" {
			return index
		}
	}
	return len(messages)
}

func deltaContent(messages []audit.Message, start int, roleMode string) (string, int) {
	if start < 0 {
		start = 0
	}
	if start > len(messages) {
		start = len(messages)
	}
	parts := make([]string, 0)
	count := 0
	for _, message := range messages[start:] {
		if !roleAllowed(message.Role, roleMode) {
			continue
		}
		content := normalizeContent(message.Content)
		if content == "" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(message.Role))
		parts = append(parts, fmt.Sprintf("[角色:%s]\n%s", role, content))
		count++
	}
	return strings.Join(parts, "\n\n---\n\n"), count
}

func roleAllowed(role, mode string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	if mode == RoleAll {
		return true
	}
	if role == "user" || role == "human" || role == "input" {
		return true
	}
	return mode == RoleUserTool && (role == "tool" || role == "function")
}

func normalizeContent(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.TrimSpace(value)
}

func (m *Manager) insertPlannedEntries(ctx context.Context, jobID int64, entries []plannedEntry) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, entry := range entries {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO review_job_entries (
			job_id, audit_entry_id, token_id, created_at, request_model, parent_audit_entry_id,
			delta_start_index, delta_message_count, content_hash, content_chars, status, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
			jobID, entry.AuditEntryID, entry.TokenID, entry.CreatedAt, entry.Model,
			entry.ParentEntryID, entry.DeltaStart, entry.DeltaMessageCount,
			entry.ContentHash, entry.ContentChars, now); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (m *Manager) ensureJobActive(ctx context.Context, jobID int64, expected string) error {
	var status string
	if err := m.db.QueryRowContext(ctx, `SELECT status FROM review_jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		return err
	}
	if status != expected {
		return fmt.Errorf("job stopped: %s", status)
	}
	return nil
}
