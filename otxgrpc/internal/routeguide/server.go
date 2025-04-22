package routeguide

import (
	context "context"
)

type Server struct {
	UnimplementedRouteGuideServer
	Context context.Context
}

func (s *Server) GetFeature(ctx context.Context, point *Point) (*Feature, error) {
	s.Context = ctx
	return nil, nil
}

func (s *Server) ListFeatures(rect *Rectangle, stream RouteGuide_ListFeaturesServer) error {
	s.Context = stream.Context()
	return nil
}

func (s *Server) RecordRoute(stream RouteGuide_RecordRouteServer) error {
	s.Context = stream.Context()
	return nil
}

func (s *Server) RouteChat(stream RouteGuide_RouteChatServer) error {
	s.Context = stream.Context()
	return nil
}
