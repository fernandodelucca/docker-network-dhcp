package util

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestAwaitCondition_TrueOnFirstCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	var calls int32
	err := AwaitCondition(ctx, func() (bool, error) {
		atomic.AddInt32(&calls, 1)
		return true, nil
	}, 10*time.Millisecond)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1 (should return on first true)", calls)
	}
}

func TestAwaitCondition_RetriesUntilTrue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	var calls int32
	err := AwaitCondition(ctx, func() (bool, error) {
		n := atomic.AddInt32(&calls, 1)
		return n >= 3, nil
	}, 5*time.Millisecond)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestAwaitCondition_PropagatesError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	wantErr := errors.New("cond explodes")
	err := AwaitCondition(ctx, func() (bool, error) {
		return false, wantErr
	}, 5*time.Millisecond)

	if !errors.Is(err, wantErr) {
		t.Errorf("got error %v, want %v", err, wantErr)
	}
}

func TestAwaitCondition_ContextTimeoutReturnsErr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := AwaitCondition(ctx, func() (bool, error) {
		return false, nil // never satisfied
	}, 10*time.Millisecond)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want context.DeadlineExceeded", err)
	}
}

func TestAwaitCondition_ContextCancelReturnsErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := AwaitCondition(ctx, func() (bool, error) {
		return false, nil
	}, 5*time.Millisecond)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}
