package persistence

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"
)

// Read-modify-write transactions from many goroutines — the shape of every revisioned update, claim and
// rotation in FastTask — must all commit. A deferred transaction that read before another writer
// committed cannot upgrade to a write lock, and SQLite reports that as "database is locked" without
// consulting busy_timeout; an IMMEDIATE transaction waits for the lock instead.
func TestConcurrentReadModifyWriteTransactionsAllCommit(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	if err := store.DB.Exec("CREATE TABLE counters (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Exec("INSERT INTO counters (id, n) VALUES (1, 0)").Error; err != nil {
		t.Fatal(err)
	}
	const workers, increments = 16, 25
	var failures atomic.Int32
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < increments; i++ {
				err := store.Transaction(context.Background(), func(tx *gorm.DB) error {
					var n int
					if err := tx.Raw("SELECT n FROM counters WHERE id = 1").Scan(&n).Error; err != nil {
						return err
					}
					return tx.Exec("UPDATE counters SET n = ? WHERE id = 1", n+1).Error
				})
				if err != nil {
					failures.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	var n int
	if err := store.DB.Raw("SELECT n FROM counters WHERE id = 1").Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if failures.Load() != 0 || n != workers*increments {
		t.Fatalf("%d transactions failed; counter=%d, want %d", failures.Load(), n, workers*increments)
	}
}
