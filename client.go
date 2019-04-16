package accord

import (
	"context"
	"io"
	"path/filepath"
	"time"

	"github.com/bsm/accord/internal/cache"
	"github.com/bsm/accord/internal/proto"
	"github.com/google/uuid"
	"google.golang.org/grpc"
)

// ClientOptions contains options for the client
type ClientOptions struct {
	Owner     string        // owner, default: random UUID
	Namespace string        // namespace, default: ""
	TTL       time.Duration // TTL, default: 10 minutes
	Dir       string        // Data directory, defaults to './data'
	OnError   func(error)   // custom error handler for background tasks
}

func (o *ClientOptions) ttlSeconds() uint32 {
	return uint32(o.TTL / time.Second)
}

func (o *ClientOptions) handleError(err error) {
	if o.OnError != nil {
		o.OnError(err)
	}
}

func (o *ClientOptions) norm() *ClientOptions {
	var p ClientOptions
	if o != nil {
		p = *o
	}

	if p.Owner == "" {
		p.Owner = uuid.New().String()
	}
	if p.TTL < time.Second {
		p.TTL = 10 * time.Minute
	}
	if p.Dir == "" {
		p.Dir = "data"
	}
	return &p
}

// --------------------------------------------------------------------

// Client represents an accord client.
type Client interface {
	// Acquire acquires a named resource handle.
	Acquire(ctx context.Context, name string, meta map[string]string) (*Handle, error)
	// Close closes the connection.
	Close() error
}

type client struct {
	rpc   proto.V1Client
	opt   *ClientOptions
	cache cache.Cache
	ownCC *grpc.ClientConn
}

// RPCClient inits a new client.
func RPCClient(ctx context.Context, rpc proto.V1Client, opt *ClientOptions) (Client, error) {
	opt = opt.norm()
	cache, err := cache.OpenBadger(filepath.Join(opt.Dir, "cache"))
	if err != nil {
		return nil, err
	}

	client := &client{
		rpc:   rpc,
		opt:   opt,
		cache: cache,
	}
	if err := client.fetchDone(ctx); err != nil {
		_ = cache.Close()
		return nil, err
	}
	return client, nil
}

// WrapClient inits a new client by wrapping a gRCP client connection.
func WrapClient(ctx context.Context, cc *grpc.ClientConn, opt *ClientOptions) (Client, error) {
	return RPCClient(ctx, proto.NewV1Client(cc), opt)
}

// DialClient creates a new client connection.
func DialClient(ctx context.Context, target string, opt *ClientOptions, dialOpt ...grpc.DialOption) (Client, error) {
	cc, err := grpc.DialContext(ctx, target, dialOpt...)
	if err != nil {
		return nil, err
	}
	ci, err := WrapClient(ctx, cc, opt)
	if err != nil {
		_ = cc.Close()
		return nil, err
	}

	ci.(*client).ownCC = cc
	return ci, nil
}

// Acquire implements ClientConn interface.
func (c *client) Acquire(ctx context.Context, name string, meta map[string]string) (*Handle, error) {
	// check in cache first
	if found, err := c.cache.Contains(name); err != nil {
		return nil, err
	} else if found {
		return nil, ErrDone
	}

	// try to acquire
	res, err := c.rpc.Acquire(ctx, &proto.AcquireRequest{
		Owner:     c.opt.Owner,
		Name:      name,
		Namespace: c.opt.Namespace,
		Ttl:       c.opt.ttlSeconds(),
		Metadata:  meta,
	})
	if err != nil {
		return nil, err
	}

	switch res.Status {
	case proto.Status_HELD:
		return nil, ErrAcquired
	case proto.Status_DONE:
		if err := c.cache.Add(name); err != nil {
			return nil, err
		}
		return nil, ErrDone
	}

	handleID := uuid.Must(uuid.FromBytes(res.Handle.Id))
	return newHandle(handleID, c.rpc, res.Handle.Metadata, c.opt), nil
}

// Close implements Client interface.
func (c *client) Close() error {
	var err error
	if c.cache != nil {
		if e2 := c.cache.Close(); e2 != nil {
			err = e2
		}
	}
	if c.ownCC != nil {
		if e2 := c.ownCC.Close(); e2 != nil {
			err = e2
		}
	}
	return err
}

func (c *client) fetchDone(ctx context.Context) error {
	res, err := c.rpc.List(ctx, &proto.ListRequest{
		Filter: &proto.ListRequest_Filter{
			Prefix: c.opt.Namespace,
			Status: proto.ListRequest_Filter_DONE,
		},
	})
	if err != nil {
		return err
	}

	wb, err := c.cache.AddBatch()
	if err != nil {
		return err
	}
	defer wb.Discard()

	for {
		handle, err := res.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		if handle.Namespace == c.opt.Namespace {
			if err := wb.Add(handle.Name); err != nil {
				return err
			}
		}
	}
	return wb.Flush()
}
