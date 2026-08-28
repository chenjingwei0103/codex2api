package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// 订阅升级是真实付费操作，开关存放在 system_settings.subscription_upgrades_enabled。
// 这里刻意用独立的小 UPDATE 读写，不进 SaveSettings 的巨型 UPSERT：该列由专用
// 管理入口维护，与整行设置保存互不覆盖。
//
// 该列可为 NULL，语义是「管理员从未在后台设置过」，此时回落到环境变量默认值。
// 一旦管理员显式开关过，数据库值就是唯一权威，环境变量不再顶回去——付费开关
// 必须任何时候都能从界面关掉。

// LoadSubscriptionUpgradesEnabled 返回管理员显式保存过的开关值；从未设置返回 nil。
func (db *DB) LoadSubscriptionUpgradesEnabled(ctx context.Context) (*bool, error) {
	var raw any
	err := db.conn.QueryRowContext(ctx, `
		SELECT subscription_upgrades_enabled
		FROM system_settings
		WHERE id = 1
	`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return normalizeNullableBool(raw), nil
}

// SaveSubscriptionUpgradesEnabled 持久化开关值，写入后该值即为权威。
func (db *DB) SaveSubscriptionUpgradesEnabled(ctx context.Context, enabled bool) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id) VALUES (1)
			ON CONFLICT (id) DO NOTHING
		`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `
			UPDATE system_settings
			SET subscription_upgrades_enabled = $1
			WHERE id = 1
		`, enabled)
		return err
	})
}

// normalizeNullableBool 兼容两种驱动：Postgres 回 bool，SQLite 回 int64/[]byte。
func normalizeNullableBool(raw any) *bool {
	switch value := raw.(type) {
	case nil:
		return nil
	case bool:
		return &value
	case int64:
		enabled := value != 0
		return &enabled
	case float64:
		enabled := value != 0
		return &enabled
	case []byte:
		return parseNullableBoolText(string(value))
	case string:
		return parseNullableBoolText(value)
	default:
		return nil
	}
}

func parseNullableBoolText(text string) *bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	enabled, err := strconv.ParseBool(trimmed)
	if err != nil {
		return nil
	}
	return &enabled
}
