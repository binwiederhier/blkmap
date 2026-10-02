package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"heckel.io/blkmap/examples/grpc-remote/remotepb"
	"heckel.io/blkmap/source"
)

// server exports the files of one directory. Open walks SEEK_DATA/SEEK_HOLE so the client
// learns the holes once and never asks for them; Read is a plain pread.
type server struct {
	remotepb.UnimplementedRemoteServer
	dir string
}

func (s *server) Open(ctx context.Context, req *remotepb.OpenRequest) (*remotepb.OpenResponse, error) {
	path, err := s.resolve(req.Name)
	if err != nil {
		return nil, err
	}
	f, err := source.OpenFile(path, 0, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "%s: no such file", req.Name)
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: %v", req.Name, err)
	}
	defer f.Close()
	extents, err := source.Extents(f)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: extents: %v", req.Name, err)
	}
	if len(extents) > maxExtents {
		return nil, status.Errorf(codes.ResourceExhausted, "%s: %d extents, more than the %d the protocol carries", req.Name, len(extents), maxExtents)
	}
	resp := &remotepb.OpenResponse{Size: f.Size()}
	for _, e := range extents {
		resp.Data = append(resp.Data, &remotepb.Extent{Offset: e.Offset, Length: e.Length})
	}
	return resp, nil
}

func (s *server) Read(ctx context.Context, req *remotepb.ReadRequest) (*remotepb.ReadResponse, error) {
	path, err := s.resolve(req.Name)
	if err != nil {
		return nil, err
	}
	if req.Length < 0 || req.Length > maxRead || req.Offset < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "offset %d length %d", req.Offset, req.Length)
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "%s: no such file", req.Name)
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: %v", req.Name, err)
	}
	defer f.Close()
	buf := make([]byte, req.Length)
	n, err := f.ReadAt(buf, req.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, status.Errorf(codes.Internal, "%s: read: %v", req.Name, err)
	}
	return &remotepb.ReadResponse{Data: buf[:n]}, nil
}

// resolve keeps names inside the export directory, symlinks included.
func (s *server) resolve(name string) (string, error) {
	clean := filepath.Clean("/" + name)
	if name == "" || strings.Contains(name, "..") || clean == "/" {
		return "", status.Errorf(codes.InvalidArgument, "bad name %q", name)
	}
	path := filepath.Join(s.dir, clean)
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", status.Errorf(codes.NotFound, "%s: no such file", name)
	} else if err != nil {
		return "", status.Errorf(codes.Internal, "%s: %v", name, err)
	}
	root, err := filepath.EvalSymlinks(s.dir)
	if err != nil || (real != root && !strings.HasPrefix(real, root+string(filepath.Separator))) {
		return "", status.Errorf(codes.PermissionDenied, "%s: outside the export", name)
	}
	return real, nil
}
