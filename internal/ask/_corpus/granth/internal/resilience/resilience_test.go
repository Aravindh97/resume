package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

func noSleep(context.Context, time.Duration) error { return nil }

func TestRetryStopsOnPermanent(t *testing.T) {
	calls := 0
	r := Retry{Attempts: 5, Base: time.Millisecond, Max: time.Millisecond, Sleep: noSleep}
	err := r.Do(context.Background(), func(context.Context) error {
		calls++
		return Permanent{errors.New("bad request")}
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}

func TestRetryRecoversFromTransient(t *testing.T) {
	calls := 0
	r := Retry{Attempts: 5, Base: time.Millisecond, Max: time.Millisecond, Sleep: noSleep}
	err := r.Do(context.Background(), func(context.Context) error {
		if calls++; calls < 3 {
			return errors.New("503")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}

func TestBreakerOpensAndProbes(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBreaker(3, 10*time.Second)
	b.Now = func() time.Time { return now }
	fail := func(context.Context) error { return errors.New("down") }
	ok := func(context.Context) error { return nil }
	for i := 0; i < 3; i++ {
		_ = b.Do(context.Background(), fail)
	}
	if err := b.Do(context.Background(), ok); !errors.Is(err, ErrOpen) {
		t.Fatalf("breaker should be open, got %v", err)
	}
	now = now.Add(11 * time.Second)
	if err := b.Do(context.Background(), ok); err != nil {
		t.Fatalf("probe after cooldown should pass, got %v", err)
	}
	if err := b.Do(context.Background(), ok); err != nil {
		t.Fatalf("breaker should be closed after a good probe, got %v", err)
	}
}

func TestPermanentErrorsDoNotOpenBreaker(t *testing.T) {
	b := NewBreaker(2, time.Minute)
	for i := 0; i < 5; i++ {
		_ = b.Do(context.Background(), func(context.Context) error { return Permanent{errors.New("400")} })
	}
	if err := b.Do(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("got %v", err)
	}
}
