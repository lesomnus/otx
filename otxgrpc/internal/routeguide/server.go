package routeguide

import (
	context "context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Server is a RouteGuide for tests. Every RPC drains whatever the client sends
// and answers with a known number of messages, so that a test can assert on the
// exact payload counts and sizes an RPC produced.
//
// The exported fields are read by every RPC and are not guarded; set them
// before the first call.
type Server struct {
	UnimplementedRouteGuideServer

	// Err, when not nil, is returned by every RPC in place of a normal result.
	// A streaming RPC still drains its input first, so that a failing RPC is
	// distinguishable from a successful one only by its status.
	Err error

	// NumSend is how many messages ListFeatures and RouteChat send back.
	// RecordRoute always answers with a single summary and GetFeature with a
	// single feature.
	NumSend int

	// Entered, when not nil, receives once as every RPC starts, so that a test
	// can act while an RPC is in flight. Give it a buffer unless something is
	// reading from it.
	Entered chan<- struct{}

	// Release, when not nil, is waited on before every RPC answers. The wait
	// ends early once the RPC context is done, so that a test can hold an RPC
	// open and then cancel it.
	Release <-chan struct{}

	mu       sync.Mutex
	ctx      context.Context
	num_recv int
}

// Context returns the context of the RPC the server handled last, or nil if it
// has handled none.
func (s *Server) Context() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ctx
}

// NumRecv reports how many messages the RPC the server handled last received.
func (s *Server) NumRecv() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.num_recv
}

func (s *Server) GetFeature(ctx context.Context, point *Point) (*Feature, error) {
	s.begin(ctx)
	s.recv()
	s.hold(ctx)

	if s.Err != nil {
		return nil, s.Err
	}

	return &Feature{Name: "feature", Location: point}, nil
}

func (s *Server) ListFeatures(rect *Rectangle, stream RouteGuide_ListFeaturesServer) error {
	ctx := stream.Context()
	s.begin(ctx)
	s.recv()
	s.hold(ctx)

	for i := range s.NumSend {
		f := &Feature{
			Name:     fmt.Sprintf("feature-%d", i),
			Location: rect.GetLo(),
		}
		if err := stream.Send(f); err != nil {
			return err
		}
	}

	return s.Err
}

func (s *Server) RecordRoute(stream RouteGuide_RecordRouteServer) error {
	ctx := stream.Context()
	s.begin(ctx)

	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		s.recv()
	}
	s.hold(ctx)

	if s.Err != nil {
		return s.Err
	}

	return stream.SendAndClose(&RouteSummary{PointCount: int32(s.NumRecv())})
}

func (s *Server) RouteChat(stream RouteGuide_RouteChatServer) error {
	ctx := stream.Context()
	s.begin(ctx)

	// The whole input is drained before anything is sent back, so that the
	// number of messages crossing in each direction does not depend on how the
	// two sides interleave.
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		s.recv()
	}
	s.hold(ctx)

	for i := range s.NumSend {
		n := &RouteNote{Message: fmt.Sprintf("note-%d", i)}
		if err := stream.Send(n); err != nil {
			return err
		}
	}

	return s.Err
}

// begin records the context of a starting RPC and reports that it started.
func (s *Server) begin(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.num_recv = 0
	s.mu.Unlock()

	if s.Entered != nil {
		s.Entered <- struct{}{}
	}
}

func (s *Server) recv() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.num_recv++
}

// hold blocks an RPC that is ready to answer, if a test asked it to.
func (s *Server) hold(ctx context.Context) {
	if s.Release == nil {
		return
	}

	select {
	case <-s.Release:
	case <-ctx.Done():
	}
}
