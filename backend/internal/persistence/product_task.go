package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ly-route/backend/internal/product"
)

const (
	ProductTaskUpgrade    = "upgrade"
	ProductTaskConversion = "conversion"
)

var (
	ErrInvalidProductTask  = errors.New("invalid product task")
	ErrProductTaskNotFound = errors.New("product task not found")
)

// ProductTask is a persisted, product-scoped long-running operation. Upgrade
// and conversion tasks share the table so a conversion can be audited and
// resumed after a control process restart.
type ProductTask struct {
	ID        string
	Product   product.ID
	TaskType  string
	State     string
	Payload   json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (s *Store) SaveProductTask(ctx context.Context, task ProductTask) error {
	profile, err := product.ParseProfile(task.Product.String())
	if err != nil || profile.ID() != s.productID {
		return fmt.Errorf("%w: task product %q does not match store %q", ErrInvalidProductTask, task.Product, s.productID)
	}
	if task.ID == "" || (task.TaskType != ProductTaskUpgrade && task.TaskType != ProductTaskConversion) || task.State == "" || len(task.Payload) == 0 || !json.Valid(task.Payload) {
		return fmt.Errorf("%w: id, type, state and valid payload are required", ErrInvalidProductTask)
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = task.CreatedAt
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO product_tasks (id, product_id, task_type, state, payload_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  state = excluded.state,
  payload_json = excluded.payload_json,
  updated_at = excluded.updated_at
WHERE product_tasks.product_id = excluded.product_id
  AND product_tasks.task_type = excluded.task_type`,
		task.ID, task.Product.String(), task.TaskType, task.State, string(task.Payload), encodeTime(task.CreatedAt), encodeTime(task.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save product task: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect saved product task: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: task id belongs to another product or task type", ErrInvalidProductTask)
	}
	return nil
}

func (s *Store) ProductTask(ctx context.Context, id string) (ProductTask, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, product_id, task_type, state, payload_json, created_at, updated_at
FROM product_tasks WHERE id = ?`, id)
	task, err := scanProductTask(row)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrProductTaskNotFound) {
			return ProductTask{}, ErrProductTaskNotFound
		}
		return ProductTask{}, fmt.Errorf("read product task: %w", err)
	}
	if task.Product != s.productID {
		return ProductTask{}, ErrProductTaskNotFound
	}
	return task, nil
}

func (s *Store) ProductTasks(ctx context.Context, taskType string) ([]ProductTask, error) {
	query := `
SELECT id, product_id, task_type, state, payload_json, created_at, updated_at
FROM product_tasks WHERE product_id = ?`
	args := []any{s.productID.String()}
	if taskType != "" {
		query += ` AND task_type = ?`
		args = append(args, taskType)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list product tasks: %w", err)
	}
	defer rows.Close()
	var tasks []ProductTask
	for rows.Next() {
		task, err := scanProductTask(rows)
		if err != nil {
			return nil, err
		}
		if task.Product == s.productID {
			tasks = append(tasks, task)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list product tasks: %w", err)
	}
	return tasks, nil
}

func scanProductTask(row scanner) (ProductTask, error) {
	var task ProductTask
	var productID, payload, createdAt, updatedAt string
	if err := row.Scan(&task.ID, &productID, &task.TaskType, &task.State, &payload, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ProductTask{}, ErrProductTaskNotFound
		}
		return ProductTask{}, convertScanError(err)
	}
	profile, err := product.ParseProfile(productID)
	if err != nil {
		return ProductTask{}, fmt.Errorf("decode product task product: %w", err)
	}
	task.Product = profile.ID()
	task.Payload = json.RawMessage(payload)
	task.CreatedAt = decodeTime(createdAt)
	task.UpdatedAt = decodeTime(updatedAt)
	return task, nil
}
