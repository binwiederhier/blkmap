package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"heckel.io/blkmap/examples/grpc-remote/remotepb"
	"heckel.io/blkmap/source"
)

const (
	// maxRead bounds one Read RPC; blkmap requests are at most 1 MiB.
	maxRead     = 1 << 20
	readTimeout = 30 * time.Second
)

// remoteFile is a read-only source.Source for one exported file. Holes come from the map the
// server sent at Open, applied by source.WithMap, so this type only ever reads data; the
// read-ahead wrapper in between turns a sequential reader's requests into 1 MiB fetches and
// keeps several in flight.
type remoteFile struct {
	client remotepb.RemoteClient
	name   string
	size   int64
}

// openRemote opens name on the server and returns it as a Source with holes attached.
func openRemote(ctx context.Context, conn *grpc.ClientConn, name string) (source.Source, error) {
	client := remotepb.NewRemoteClient(conn)
	resp, err := client.Open(ctx, &remotepb.OpenRequest{Name: name})
	if status.Code(err) == codes.NotFound {
		return nil, fmt.Errorf("%s: %w", name, source.ErrNotFound)
	} else if err != nil {
		return nil, err
	}
	extents := make([]source.Range, 0, len(resp.Data))
	for _, e := range resp.Data {
		extents = append(extents, source.Range{Offset: e.Offset, Length: e.Length})
	}
	m, err := source.NewMap(extents)
	if err != nil {
		return nil, err
	}
	return source.WithMap(source.NewReadAhead(&remoteFile{client: client, name: name, size: resp.Size}), m, 0), nil
}

func (r *remoteFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) {
		ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
		resp, err := r.client.Read(ctx, &remotepb.ReadRequest{Name: r.name, Offset: off + int64(n), Length: int64(min(len(p)-n, maxRead))})
		cancel()
		if status.Code(err) == codes.NotFound {
			return n, fmt.Errorf("%s: %w", r.name, source.ErrNotFound)
		} else if err != nil {
			return n, err
		}
		if len(resp.Data) == 0 {
			break
		}
		n += copy(p[n:], resp.Data)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *remoteFile) Size() int64 {
	return r.size
}

func (r *remoteFile) Close() error {
	return nil
}
