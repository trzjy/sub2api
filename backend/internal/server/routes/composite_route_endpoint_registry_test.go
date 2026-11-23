package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCompositeRouteRegistryEndpointForInboundPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "alpha search", path: "/v1/alpha/search", want: service.CompositeRouteEndpointResponses},
		{name: "realtime calls", path: "/v1/realtime/calls", want: service.CompositeRouteEndpointResponses},
		{name: "live", path: "/v1/live", want: service.CompositeRouteEndpointResponses},
		{name: "responses", path: "/v1/responses", want: service.CompositeRouteEndpointResponses},
		{name: "systemone", path: "/v1/systemone", want: service.CompositeRouteEndpointAny},
		{name: "count tokens", path: "/v1/messages/count_tokens", want: service.CompositeRouteEndpointCountTokens},
		{name: "messages", path: "/v1/messages", want: service.CompositeRouteEndpointMessages},
		{name: "chat completions", path: "/v1/chat/completions", want: service.CompositeRouteEndpointChatCompletions},
		{name: "embeddings", path: "/v1/embeddings", want: service.CompositeRouteEndpointEmbeddings},
		{name: "images generations", path: "/v1/images/generations", want: service.CompositeRouteEndpointImages},
		{name: "videos generations", path: "/v1/videos/generations", want: service.CompositeRouteEndpointAny},
		{name: "gemini", path: "/v1beta/models/x:generateContent", want: service.CompositeRouteEndpointGemini},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, compositeRouteEndpointForPath(tt.path))
		})
	}
}
