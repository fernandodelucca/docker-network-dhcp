package udhcpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"

	"github.com/fernandodelucca/docker-network-dhcp/pkg/util"
)

const (
	DefaultHandler = "/usr/lib/net-dhcp/udhcpc-handler"
	VendorID       = "docker-net-dhcp"
)

type DHCPClientOptions struct {
	Hostname  string
	V6        bool
	Once      bool
	Namespace string

	HandlerScript string
}

// DHCPClient represents a udhcpc(6) client
type DHCPClient struct {
	Opts *DHCPClientOptions

	cmd        *exec.Cmd
	eventPipe  io.ReadCloser
	stderrPipe io.ReadCloser
}

// NewDHCPClient creates a new udhcpc(6) client
func NewDHCPClient(iface string, opts *DHCPClientOptions) (*DHCPClient, error) {
	if opts.HandlerScript == "" {
		opts.HandlerScript = DefaultHandler
	}

	path := "udhcpc"
	if opts.V6 {
		path = "udhcpc6"
	}
	c := &DHCPClient{
		Opts: opts,
		cmd:  exec.Command(path, "-f", "-i", iface, "-s", opts.HandlerScript),
	}

	stderrPipe, err := c.cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to set up udhcpc stderr pipe: %w", err)
	}
	c.stderrPipe = stderrPipe

	if c.eventPipe, err = c.cmd.StdoutPipe(); err != nil {
		return nil, fmt.Errorf("failed to set up udhcpc stdout pipe: %w", err)
	}

	if opts.Once {
		c.cmd.Args = append(c.cmd.Args, "-q")
	} else {
		// Release IP address on exit
		c.cmd.Args = append(c.cmd.Args, "-R")
	}

	if opts.Hostname != "" {
		hostnameOpt := "hostname:" + opts.Hostname
		if opts.V6 {
			// Encode the FQDN for DHCPv6 (udhcpc6 option 0x27, RFC4704)
			var data bytes.Buffer
			binary.Write(&data, binary.BigEndian, uint8(0b0001)) // S bit set
			binary.Write(&data, binary.BigEndian, uint8(len(opts.Hostname)))
			data.WriteString(opts.Hostname)
			hostnameOpt = "0x27:" + hex.EncodeToString(data.Bytes())
		}
		c.cmd.Args = append(c.cmd.Args, "-x", hostnameOpt)
	}

	// Vendor ID string option is not available for udhcpc6
	if !opts.V6 {
		c.cmd.Args = append(c.cmd.Args, "-V", VendorID)
	}

	log.WithField("cmd", c.cmd).Trace("new udhcpc client")

	return c, nil
}

// Start starts udhcpc(6)
func (c *DHCPClient) Start() (chan Event, error) {
	if c.Opts.Namespace != "" {
		// Lock the OS Thread so we don't accidentally switch namespaces
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		origNS, err := netns.Get()
		if err != nil {
			return nil, fmt.Errorf("failed to open current network namespace: %w", err)
		}
		defer origNS.Close()

		ns, err := netns.GetFromPath(c.Opts.Namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to open network namespace `%v`: %w", c.Opts.Namespace, err)
		}
		defer ns.Close()

		if err := netns.Set(ns); err != nil {
			return nil, fmt.Errorf("failed to enter network namespace: %w", err)
		}
		defer netns.Set(origNS)
	}

	if err := c.cmd.Start(); err != nil {
		return nil, err
	}

	// Launch stderr relay only after successful Start() to avoid goroutine leak
	// on failed starts (pipe would never close if process never started).
	go io.Copy(log.StandardLogger().WriterLevel(log.DebugLevel), c.stderrPipe)

	events := make(chan Event)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(c.eventPipe)
		for scanner.Scan() {
			log.WithField("line", string(scanner.Bytes())).Trace("udhcpc handler line")

			var event Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				log.WithError(err).Warn("Failed to decode udhcpc event")
				continue
			}
			events <- event
		}
		// Log I/O errors so they're distinguishable from clean EOF
		if err := scanner.Err(); err != nil {
			log.WithError(err).Warn("udhcpc event pipe read error")
		}
	}()

	return events, nil
}

// Drain reaps the exit status of a process that has already crashed on its own.
// Call this after detecting that the event channel was closed unexpectedly,
// to avoid leaving a zombie process.
func (c *DHCPClient) Drain() {
	_ = c.cmd.Wait()
}

// Finish sends SIGTERM to udhcpc(6) and waits for it to exit.
func (c *DHCPClient) Finish(ctx context.Context) error {
	if !c.Opts.Once {
		if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("failed to send SIGTERM to udhcpc: %w", err)
		}
	}

	// Buffered(1) so the goroutine can exit even if ctx fires first.
	errChan := make(chan error, 1)
	go func() {
		errChan <- c.cmd.Wait()
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		if err := c.cmd.Process.Kill(); err != nil {
			log.WithError(err).Warn("udhcpc SIGKILL failed — process may have already exited")
		}
		return ctx.Err()
	}
}

// GetIP is a convenience function that runs udhcpc(6) once and returns the IP obtained.
func GetIP(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, error) {
	opts.Once = true
	client, err := NewDHCPClient(iface, opts)
	if err != nil {
		return Info{}, fmt.Errorf("failed to create DHCP client: %w", err)
	}

	events, err := client.Start()
	if err != nil {
		return Info{}, fmt.Errorf("failed to start DHCP client: %w", err)
	}

	var info *Info
	done := make(chan struct{})
	go func() {
		for {
			select {
			case event := <-events:
				switch event.Type {
				case "bound", "renew":
					info = &event.Data
				}
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	if err := client.Finish(ctx); err != nil {
		return Info{}, err
	}

	if info == nil {
		return Info{}, util.ErrNoLease
	}

	return *info, nil
}
