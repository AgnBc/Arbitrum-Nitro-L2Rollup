package discretetimeboost

import (
	"container/heap"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

var _ heap.Interface = (*timeBoostableTxs)(nil)

type mockTx struct {
	_id        string
	_timestamp time.Time
	_bid       uint64
	_innerTx   *types.Transaction
}

func (m *mockTx) id() string {
	return m._id
}

func (m *mockTx) bid() uint64 {
	return m._bid
}

func (m *mockTx) timestamp() time.Time {
	return m._timestamp
}

func (m *mockTx) innerTx() *types.Transaction {
	return m._innerTx
}

func TestTxPriorityQueue(t *testing.T) {
	txs := timeBoostableTxs(make([]boostableTx, 0))
	heap.Init(&txs)

	t.Run("order by bid", func(t *testing.T) {
		now := time.Now()
		heap.Push(&txs, &mockTx{
			_bid:       0,
			_timestamp: now,
		})
		heap.Push(&txs, &mockTx{
			_bid:       100,
			_timestamp: now.Add(time.Millisecond * 100),
		})
		got := make([]*mockTx, 0)
		for txs.Len() > 0 {
			tx := heap.Pop(&txs).(*mockTx)
			got = append(got, tx)
		}
		if len(got) != 2 {
			t.Fatalf("Wanted %d, got %d", 2, len(got))
		}
		if got[0]._bid != uint64(100) {
			t.Fatalf("Wanted %d, got %d", 100, got[0]._bid)
		}
		if got[1]._bid != uint64(0) {
			t.Fatalf("Wanted %d, got %d", 0, got[1]._bid)
		}
	})
	t.Run("tiebreak by timestamp", func(t *testing.T) {
		now := time.Now()
		heap.Push(&txs, &mockTx{
			_id:        "a",
			_bid:       100,
			_timestamp: now.Add(time.Millisecond * 100),
		})
		heap.Push(&txs, &mockTx{
			_id:        "b",
			_bid:       100,
			_timestamp: now,
		})
		got := make([]*mockTx, 0)
		for txs.Len() > 0 {
			tx := heap.Pop(&txs).(*mockTx)
			got = append(got, tx)
		}
		if len(got) != 2 {
			t.Fatalf("Wanted %d, got %d", 2, len(got))
		}
		if got[0]._id != "b" {
			t.Fatalf("Wanted %s, got %s", "b", got[0]._id)
		}
		if got[1]._id != "a" {
			t.Fatalf("Wanted %s, got %s", "a", got[1]._id)
		}
	})
	t.Run("no bid, order by timestamp", func(t *testing.T) {
		now := time.Now()
		heap.Push(&txs, &mockTx{
			_id:        "a",
			_bid:       0,
			_timestamp: now.Add(time.Millisecond * 100),
		})
		heap.Push(&txs, &mockTx{
			_id:        "b",
			_bid:       0,
			_timestamp: now,
		})
		heap.Push(&txs, &mockTx{
			_id:        "c",
			_bid:       0,
			_timestamp: now.Add(time.Millisecond * 200),
		})
		got := make([]*mockTx, 0)
		for txs.Len() > 0 {
			tx := heap.Pop(&txs).(*mockTx)
			got = append(got, tx)
		}
		if len(got) != 3 {
			t.Fatalf("Wanted %d, got %d", 3, len(got))
		}
		if got[0]._id != "b" {
			t.Fatalf("Wanted %s, got %s", "b", got[0]._id)
		}
		if got[1]._id != "a" {
			t.Fatalf("Wanted %s, got %s", "a", got[1]._id)
		}
		if got[2]._id != "c" {
			t.Fatalf("Wanted %s, got %s", "a", got[2]._id)
		}
	})
}
