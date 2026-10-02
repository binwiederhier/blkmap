package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"heckel.io/blkmap/examples/grpc-remote/remotepb"
	"heckel.io/blkmap/source"
)

func pattern(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*7 + i/256)
	}
	return p
}

// testServer runs the export server over an in-memory connection.
func testServer(t *testing.T, dir string) *grpc.ClientConn {
	t.Helper()
	l := bufconn.Listen(1 << 20)
	s := grpc.NewServer(grpc.MaxSendMsgSize(2 * maxRead))
	remotepb.RegisterRemoteServer(s, &server{dir: dir})
	go s.Serve(l)
	t.Cleanup(s.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(2*maxRead)))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestRemoteSparseFile(t *testing.T) {
	dir := t.TempDir()
	// 8 MiB sparse file with 64 KiB of data at 2 MiB and 1 MiB of data at 6 MiB
	f, err := os.Create(filepath.Join(dir, "sparse.img"))
	require.NoError(t, err)
	require.NoError(t, f.Truncate(8<<20))
	_, err = f.WriteAt(pattern(64<<10), 2<<20)
	require.NoError(t, err)
	_, err = f.WriteAt(pattern(1<<20), 6<<20)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	conn := testServer(t, dir)
	src, err := openRemote(context.Background(), conn, "sparse.img")
	require.NoError(t, err)
	defer src.Close()
	assert.Equal(t, int64(8<<20), src.Size())
	// The holes are known without a single read
	holes, err := source.Holes(src, 0, 8<<20)
	require.NoError(t, err)
	require.Len(t, holes, 3)
	assert.Equal(t, int64(0), holes[0].Offset)
	assert.GreaterOrEqual(t, holes[0].Length, int64(2<<20-64<<10))
	assert.Equal(t, int64(8<<20), holes[2].Offset+holes[2].Length)
	data, err := source.Extents(src)
	require.NoError(t, err)
	assert.Len(t, data, 2)
	// Reads: a hole (served locally), data, and a 1 MiB request straddling data and hole
	p := make([]byte, 4096)
	_, err = src.ReadAt(p, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, 4096), p)
	_, err = src.ReadAt(p, 2<<20+100)
	require.NoError(t, err)
	assert.Equal(t, pattern(64 << 10)[100:4196], p)
	big := make([]byte, 1<<20)
	_, err = src.ReadAt(big, 6<<20+512<<10)
	require.NoError(t, err)
	assert.Equal(t, pattern(1 << 20)[512<<10:], big[:512<<10])
	assert.Equal(t, make([]byte, 512<<10), big[512<<10:])
	// Past the end
	n, err := src.ReadAt(p, 8<<20-100)
	assert.Error(t, err)
	assert.Equal(t, 100, n)
}

func TestRemoteErrors(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain"), pattern(4096), 0600))
	conn := testServer(t, dir)
	_, err := openRemote(context.Background(), conn, "missing")
	require.Error(t, err)
	assert.ErrorIs(t, err, source.ErrNotFound)
	for _, bad := range []string{"", "../etc/passwd", "a/../../b"} {
		_, err := openRemote(context.Background(), conn, bad)
		require.Error(t, err, bad)
	}
	// A plain (non-sparse) file is one data extent
	src, err := openRemote(context.Background(), conn, "plain")
	require.NoError(t, err)
	data, err := source.Extents(src)
	require.NoError(t, err)
	assert.Equal(t, []source.Range{{Offset: 0, Length: 4096}}, data)
}
