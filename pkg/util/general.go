package util

import (
	"context"
	"time"
)

func AwaitCondition(ctx context.Context, cond func() (bool, error), interval time.Duration) error {
	errChan := make(chan error, 1)
	go func() {
		for {
			if ctx.Err() != nil {
				errChan <- ctx.Err()
				return
			}
			ok, err := cond()
			if err != nil {
				errChan <- err
				return
			}

			if ok {
				errChan <- nil
				return
			}

			select {
			case <-ctx.Done():
				errChan <- ctx.Err()
				return
			case <-time.After(interval):
			}
		}
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
