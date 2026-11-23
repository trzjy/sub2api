//go:build unit

package service

import "testing"

func TestSortedCompositeOwnershipPlatforms(t *testing.T) {
	tests := []struct {
		name      string
		platforms map[string]struct{}
		want      []string
	}{
		{
			name:      "empty map returns empty slice",
			platforms: map[string]struct{}{},
			want:      []string{},
		},
		{
			name:      "single platform",
			platforms: map[string]struct{}{"anthropic": {}},
			want:      []string{"anthropic"},
		},
		{
			name: "multiple platforms are sorted",
			platforms: map[string]struct{}{
				"gemini":    {},
				"anthropic": {},
				"openai":    {},
			},
			want: []string{"anthropic", "gemini", "openai"},
		},
		{
			name: "duplicate entries collapse and stay sorted",
			platforms: map[string]struct{}{
				"z-platform": {},
				"a-platform": {},
				"m-platform": {},
			},
			want: []string{"a-platform", "m-platform", "z-platform"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sortedCompositeOwnershipPlatforms(tt.platforms)
			if len(got) != len(tt.want) {
				t.Fatalf("length = %d, want %d (got %v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("element[%d] = %q, want %q (got %v)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}
