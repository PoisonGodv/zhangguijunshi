package gateway

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestBudget_Memory(t *testing.T) {
	b := NewMemoryBudget(2.0)
	ctx := context.Background()

	spent, _ := b.Spent(ctx, 1)
	if spent != 0 {
		t.Fatalf("initial spent = %v, want 0", spent)
	}
	_ = b.AddCost(ctx, 1, 0.5)
	_ = b.AddCost(ctx, 1, 1.0)
	spent, _ = b.Spent(ctx, 1)
	if !approx(spent, 1.5, 1e-9) {
		t.Fatalf("spent = %v, want 1.5", spent)
	}
	exceeded, _ := b.Exceeded(ctx, 1)
	if exceeded {
		t.Fatal("should not be exceeded at 1.5")
	}
	_ = b.AddCost(ctx, 1, 0.6) // 2.1 -> exceeded
	exceeded, _ = b.Exceeded(ctx, 1)
	if !exceeded {
		t.Fatal("should be exceeded at 2.1")
	}
	// Other tenant unaffected (isolation).
	exceeded2, _ := b.Exceeded(ctx, 2)
	if exceeded2 {
		t.Fatal("tenant 2 should not be exceeded")
	}
}

func TestBudget_Redis(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c.Close()
	b := NewRedisBudget(c, 2.0)
	ctx := context.Background()

	_ = b.AddCost(ctx, 42, 1.0)
	_ = b.AddCost(ctx, 42, 1.5) // 2.5 -> exceeded
	spent, _ := b.Spent(ctx, 42)
	if !approx(spent, 2.5, 1e-9) {
		t.Fatalf("spent = %v, want 2.5", spent)
	}
	exceeded, _ := b.Exceeded(ctx, 42)
	if !exceeded {
		t.Fatal("should be exceeded")
	}
}
