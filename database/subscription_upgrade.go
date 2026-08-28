package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrSubscriptionUpgradeIdempotencyConflict = errors.New("subscription upgrade idempotency key already exists")

// ErrSubscriptionUpgradeInFlight 表示同一账号已有一次付费提交尚未落定。
var ErrSubscriptionUpgradeInFlight = errors.New("subscription upgrade already in flight for this account")

// SubscriptionUpgradeStatusSubmitting 是付费 POST 前写入的前置状态。它同时充当
// 跨实例互斥：部分唯一索引保证同一账号只能存在一行 submitting。
const SubscriptionUpgradeStatusSubmitting = "submitting"

// 部分唯一索引名，用于把唯一冲突和幂等键冲突区分开。
const subscriptionUpgradeInFlightIndex = "subscription_upgrade_operations_inflight_idx"

// SubscriptionUpgradeOperation is an append-oriented payment journal. It must
// contain no OAuth credentials, cookies, payment method IDs, or card data.
type SubscriptionUpgradeOperation struct {
	ID                 string    `json:"operation_id"`
	AccountID          int64     `json:"account_id"`
	IdempotencyKeyHash string    `json:"-"`
	SourcePlan         string    `json:"source_plan"`
	TargetPlan         string    `json:"target_plan"`
	Currency           string    `json:"currency"`
	AmountDueMinor     int64     `json:"amount_due_minor"`
	Status             string    `json:"status"`
	SubmitHTTPStatus   int       `json:"submit_http_status,omitempty"`
	Detail             string    `json:"detail,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func (db *DB) ensureSubscriptionUpgradeSchema(ctx context.Context) error {
	ddl := `CREATE TABLE IF NOT EXISTS subscription_upgrade_operations (
		id TEXT PRIMARY KEY,
		account_id BIGINT NOT NULL,
		idempotency_key_hash TEXT NOT NULL,
		source_plan TEXT NOT NULL,
		target_plan TEXT NOT NULL,
		currency VARCHAR(16) NOT NULL,
		amount_due_minor BIGINT NOT NULL,
		status VARCHAR(64) NOT NULL,
		submit_http_status INT NOT NULL DEFAULT 0,
		detail TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(account_id, idempotency_key_hash)
	)`
	if db.isSQLite() {
		ddl = `CREATE TABLE IF NOT EXISTS subscription_upgrade_operations (
			id TEXT PRIMARY KEY,
			account_id INTEGER NOT NULL,
			idempotency_key_hash TEXT NOT NULL,
			source_plan TEXT NOT NULL,
			target_plan TEXT NOT NULL,
			currency TEXT NOT NULL,
			amount_due_minor INTEGER NOT NULL,
			status TEXT NOT NULL,
			submit_http_status INTEGER NOT NULL DEFAULT 0,
			detail TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(account_id, idempotency_key_hash)
		)`
	}
	if _, err := db.conn.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create subscription upgrade operation table: %w", err)
	}
	// 同一账号同时只允许一行 submitting。进程内互斥挡不住多实例，这个部分唯一
	// 索引才是真正原子的双重扣款闸门：第二个实例插不进去，也就发不出第二次付费。
	if _, err := db.conn.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS `+
		subscriptionUpgradeInFlightIndex+` ON subscription_upgrade_operations (account_id)
		WHERE status = '`+SubscriptionUpgradeStatusSubmitting+`'`); err != nil {
		return fmt.Errorf("create subscription upgrade in-flight index: %w", err)
	}
	return nil
}

// isSubscriptionUpgradeInFlightConflict 识别在途唯一索引冲突。两种驱动的报错形态
// 不同：Postgres 会带上索引名，SQLite 的部分唯一索引只报到列名，必须分别匹配。
// 复合幂等约束的冲突已被 ON CONFLICT 吞掉，这里再按列名把它排除掉以防误判。
func isSubscriptionUpgradeInFlightConflict(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	if strings.Contains(message, subscriptionUpgradeInFlightIndex) {
		return true
	}
	return strings.Contains(message, "UNIQUE constraint failed") &&
		strings.Contains(message, "subscription_upgrade_operations.account_id") &&
		!strings.Contains(message, "idempotency_key_hash")
}

func (db *DB) CreateSubscriptionUpgradeOperation(ctx context.Context, operation SubscriptionUpgradeOperation) error {
	query := `INSERT INTO subscription_upgrade_operations (
		id, account_id, idempotency_key_hash, source_plan, target_plan,
		currency, amount_due_minor, status, submit_http_status, detail
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	ON CONFLICT (account_id, idempotency_key_hash) DO NOTHING`
	return db.withSQLiteWriteLock(ctx, func() error {
		result, err := db.conn.ExecContext(ctx, query,
			operation.ID, operation.AccountID, operation.IdempotencyKeyHash,
			operation.SourcePlan, operation.TargetPlan, operation.Currency,
			operation.AmountDueMinor, operation.Status, operation.SubmitHTTPStatus,
			operation.Detail,
		)
		if err != nil {
			// ON CONFLICT 只覆盖幂等键那一个约束，在途索引的冲突会以普通错误抛出。
			if isSubscriptionUpgradeInFlightConflict(err) {
				return ErrSubscriptionUpgradeInFlight
			}
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrSubscriptionUpgradeIdempotencyConflict
		}
		return nil
	})
}

func (db *DB) UpdateSubscriptionUpgradeOperation(ctx context.Context, id, status, detail string, submitHTTPStatus int) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		result, err := db.conn.ExecContext(ctx, `UPDATE subscription_upgrade_operations
			SET status=$2, detail=$3, submit_http_status=$4, updated_at=CURRENT_TIMESTAMP
			WHERE id=$1`, id, status, detail, submitHTTPStatus)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return errors.New("subscription upgrade operation not found")
		}
		return nil
	})
}

func (db *DB) GetSubscriptionUpgradeOperation(ctx context.Context, id string) (*SubscriptionUpgradeOperation, error) {
	return db.querySubscriptionUpgradeOperation(ctx, `WHERE id=$1`, id)
}

func (db *DB) GetSubscriptionUpgradeOperationByIdempotencyHash(ctx context.Context, accountID int64, keyHash string) (*SubscriptionUpgradeOperation, error) {
	return db.querySubscriptionUpgradeOperation(ctx, `WHERE account_id=$1 AND idempotency_key_hash=$2`, accountID, keyHash)
}

func (db *DB) querySubscriptionUpgradeOperation(ctx context.Context, where string, args ...any) (*SubscriptionUpgradeOperation, error) {
	query := `SELECT id, account_id, idempotency_key_hash, source_plan, target_plan,
		currency, amount_due_minor, status, submit_http_status, detail, created_at, updated_at
		FROM subscription_upgrade_operations ` + where + ` LIMIT 1`
	var operation SubscriptionUpgradeOperation
	var createdRaw, updatedRaw any
	err := db.conn.QueryRowContext(ctx, query, args...).Scan(
		&operation.ID, &operation.AccountID, &operation.IdempotencyKeyHash,
		&operation.SourcePlan, &operation.TargetPlan, &operation.Currency,
		&operation.AmountDueMinor, &operation.Status, &operation.SubmitHTTPStatus,
		&operation.Detail, &createdRaw, &updatedRaw,
	)
	if err != nil {
		return nil, err
	}
	operation.CreatedAt, err = parseDBTimeValue(createdRaw)
	if err != nil {
		return nil, err
	}
	operation.UpdatedAt, err = parseDBTimeValue(updatedRaw)
	if err != nil {
		return nil, err
	}
	return &operation, nil
}
