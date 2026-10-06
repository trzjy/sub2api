package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// ParseFooterLinks must return an empty, non-nil slice for empty/[]/corrupt input
// (§1 user ruling: a corrupt stored value must not take down the public settings
// endpoint). It mirrors the custom_endpoints/custom_menu_items contract.
func TestParseFooterLinksEmptyInput(t *testing.T) {
	require.Equal(t, []FooterLink{}, ParseFooterLinks(""))
	require.Equal(t, []FooterLink{}, ParseFooterLinks("[]"))
	require.Equal(t, []FooterLink{}, ParseFooterLinks("   "))
}

func TestParseFooterLinksBadJSON(t *testing.T) {
	// Corrupt JSON from a hand-edited DB row must downgrade to empty, never panic.
	require.Equal(t, []FooterLink{}, ParseFooterLinks("not json"))
	require.Equal(t, []FooterLink{}, ParseFooterLinks(`{"id":`))
	require.Equal(t, []FooterLink{}, ParseFooterLinks(`[{"id":1,}]`))
}

func TestParseFooterLinksRoundTrip(t *testing.T) {
	raw := `[{"id":"reviewsite","name":"AI API中转站评测","url":"https://example.com/","sort_order":1},{"id":"other","name":"Other","url":"https://example.com/","sort_order":2}]`
	links := ParseFooterLinks(raw)
	require.Len(t, links, 2)
	require.Equal(t, "reviewsite", links[0].ID)
	require.Equal(t, "AI API中转站评测", links[0].Name)
	require.Equal(t, "https://example.com/", links[0].URL)
	require.Equal(t, 1, links[0].SortOrder)
	require.Equal(t, "other", links[1].ID)

	out, err := json.Marshal(links)
	require.NoError(t, err)
	require.JSONEq(t, raw, string(out))
}
