package comments

import (
	"Coves/internal/core/comments"
	"net/http"
)

// ServiceAdapter adapts the core comments.Service to the handler's Service interface
// This bridges the gap between HTTP-layer concerns (http.Request) and domain-layer concerns (context.Context)
type ServiceAdapter struct {
	coreService comments.Service
}

// NewServiceAdapter creates a new service adapter wrapping the core comment service
func NewServiceAdapter(coreService comments.Service) Service {
	return &ServiceAdapter{
		coreService: coreService,
	}
}

// GetComments adapts the handler request to the core service request
// Converts handler-specific GetCommentsRequest to core GetCommentsRequest
func (a *ServiceAdapter) GetComments(r *http.Request, req *GetCommentsRequest) (*comments.GetCommentsResponse, error) {
	// Convert handler request to core service request
	coreReq := &comments.GetCommentsRequest{
		PostURI:   req.PostURI,
		Sort:      req.Sort,
		Timeframe: req.Timeframe,
		Depth:     req.Depth,
		Limit:     req.Limit,
		Cursor:    req.Cursor,
		ViewerDID: req.ViewerDID,
	}

	// Call core service with request context
	return a.coreService.GetComments(r.Context(), coreReq)
}
