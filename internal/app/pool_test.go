package app

import (
	"path/filepath"
	"testing"
)

func TestPoolRoundRobinStrategy(t *testing.T) {
	store := newTestStore(t)
	first := addTestAccount(t, store, "first", 10, 100, 1)
	second := addTestAccount(t, store, "second", 10, 100, 1)
	pool := NewPool(store, []string{"glm-5.1"}, PoolStrategyRoundRobin)

	firstLease, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	secondLease, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire second: %v", err)
	}

	if firstLease.Account.ID != first || secondLease.Account.ID != second {
		t.Fatalf("round-robin got %d then %d, want %d then %d", firstLease.Account.ID, secondLease.Account.ID, first, second)
	}
}

func TestPoolFillFirstStrategy(t *testing.T) {
	store := newTestStore(t)
	first := addTestAccount(t, store, "first", 2, 100, 1)
	second := addTestAccount(t, store, "second", 2, 100, 1)
	if _, err := store.SaveModelSettings(ModelSettings{
		Models:       []string{"glm-5.1"},
		DefaultModel: "glm-5.1",
		PoolStrategy: PoolStrategyFillFirst,
	}, []string{"glm-5.1"}, PoolStrategyRoundRobin); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	pool := NewPool(store, []string{"glm-5.1"}, PoolStrategyRoundRobin)

	firstLease, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	secondLease, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire second: %v", err)
	}
	thirdLease, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire third: %v", err)
	}

	if firstLease.Account.ID != first || secondLease.Account.ID != first || thirdLease.Account.ID != second {
		t.Fatalf("fill-first got %d, %d, %d; want %d, %d, %d",
			firstLease.Account.ID, secondLease.Account.ID, thirdLease.Account.ID,
			first, first, second)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "test.sqlite3"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func addTestAccount(t *testing.T, store *Store, name string, concurrency int, priority int, weight int) int64 {
	t.Helper()
	id, err := store.AddAccount(AccountCreate{
		Name:        name,
		APIKey:      "ck_test_" + name,
		Concurrency: &concurrency,
		Priority:    &priority,
		Weight:      &weight,
	})
	if err != nil {
		t.Fatalf("add account %s: %v", name, err)
	}
	return id
}
