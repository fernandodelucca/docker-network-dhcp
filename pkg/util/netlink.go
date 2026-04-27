package util

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func AwaitNetNS(ctx context.Context, path string, interval time.Duration) (netns.NsHandle, error) {
	nsChan := make(chan netns.NsHandle, 1)
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			ns, err := netns.GetFromPath(path)
			if err == nil {
				nsChan <- ns
				return
			}
			log.WithError(err).WithField("path", path).Trace("Awaiting network namespace")
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()

	var dummy netns.NsHandle
	select {
	case ns := <-nsChan:
		return ns, nil
	case <-ctx.Done():
		return dummy, ctx.Err()
	}
}

func AwaitLinkByIndex(ctx context.Context, handle *netlink.Handle, index int, interval time.Duration) (netlink.Link, error) {
	linkChan := make(chan netlink.Link, 1)
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			link, err := handle.LinkByIndex(index)
			if err == nil {
				linkChan <- link
				return
			}
			log.WithError(err).WithField("index", index).Trace("Awaiting link by index")
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()

	var dummy netlink.Link
	select {
	case link := <-linkChan:
		return link, nil
	case <-ctx.Done():
		return dummy, ctx.Err()
	}
}
