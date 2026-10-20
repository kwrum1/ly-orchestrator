package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"ly-route/backend/internal/product"
)

func TestProductTaskPersistsStateAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "tasks.db")
	store, err := OpenForProduct(ctx, databasePath, product.Orchestrator().ID())
	if err != nil {
		t.Fatal(err)
	}
	task := ProductTask{
		ID: "upgrade-1", Product: product.Orchestrator().ID(), TaskType: ProductTaskUpgrade,
		State: "staged", Payload: json.RawMessage(`{"task":{"id":"upgrade-1"}}`),
		CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	}
	if err := store.SaveProductTask(ctx, task); err != nil {
		t.Fatalf("save task: %v", err)
	}
	task.State = "installed"
	task.UpdatedAt = task.CreatedAt.Add(time.Minute)
	if err := store.SaveProductTask(ctx, task); err != nil {
		t.Fatalf("update task: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenForProduct(ctx, databasePath, product.Orchestrator().ID())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.ProductTask(ctx, "upgrade-1")
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if loaded.State != "installed" || loaded.TaskType != ProductTaskUpgrade || loaded.Product != product.Orchestrator().ID() {
		t.Fatalf("loaded task = %#v", loaded)
	}
	tasks, err := reopened.ProductTasks(ctx, ProductTaskUpgrade)
	if err != nil || len(tasks) != 1 || tasks[0].ID != "upgrade-1" {
		t.Fatalf("listed tasks = %#v err=%v", tasks, err)
	}
	if _, err := reopened.ProductTask(ctx, "missing"); !errors.Is(err, ErrProductTaskNotFound) {
		t.Fatalf("missing task error = %v", err)
	}
}

func TestProductTaskRejectsCrossProductWritesAndReads(t *testing.T) {
	ctx := context.Background()
	gatewayStore, err := OpenForProduct(ctx, "file:"+t.Name()+"-gateway?mode=memory&cache=shared", product.Gateway().ID())
	if err != nil {
		t.Fatal(err)
	}
	defer gatewayStore.Close()
	err = gatewayStore.SaveProductTask(ctx, ProductTask{
		ID: "conversion-1", Product: product.Orchestrator().ID(), TaskType: ProductTaskConversion,
		State: "validated", Payload: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrInvalidProductTask) {
		t.Fatalf("cross-product save error = %v", err)
	}
	orchestratorStore, err := OpenForProduct(ctx, "file:"+t.Name()+"-orch?mode=memory&cache=shared", product.Orchestrator().ID())
	if err != nil {
		t.Fatal(err)
	}
	defer orchestratorStore.Close()
	if err := orchestratorStore.SaveProductTask(ctx, ProductTask{
		ID: "conversion-2", Product: product.Orchestrator().ID(), TaskType: ProductTaskConversion,
		State: "validated", Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := gatewayStore.ProductTask(ctx, "conversion-2"); !errors.Is(err, ErrProductTaskNotFound) {
		t.Fatalf("cross-product read error = %v, want not found", err)
	}
}
