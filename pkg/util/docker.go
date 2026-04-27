package util

import (
	"context"
	"time"

	"github.com/moby/moby/client"
	log "github.com/sirupsen/logrus"
)

const (
	OptionsKeyGeneric = "com.docker.network.generic"
)

func AwaitContainerInspect(ctx context.Context, docker *client.Client, id string, interval time.Duration) (client.ContainerInspectResult, error) {
	ctrChan := make(chan client.ContainerInspectResult, 1)
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			ctr, err := docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
			if err == nil {
				ctrChan <- ctr
				return
			}
			log.WithError(err).WithField("id", id).Trace("Awaiting container inspect")
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()

	var dummy client.ContainerInspectResult
	select {
	case ctr := <-ctrChan:
		return ctr, nil
	case <-ctx.Done():
		return dummy, ctx.Err()
	}
}
