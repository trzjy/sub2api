package service

import "testing"

func TestIsRegistrationEmailSuffixBlocked(t *testing.T) {
	blacklist := []string{"@mailinator.com", "*.edu.cn"}

	cases := []struct {
		email   string
		blocked bool
	}{
		{"user@mailinator.com", true},                 // exact match
		{"user@sub.mailinator.com", false},            // subdomain not blocked unless wildcard
		{"student@x.edu.cn", true},                   // wildcard match
		{"student@deep.x.edu.cn", true},              // wildcard deep match
		{"student@other.edu.com", false},             // different TLD
		{"user@gmail.com", false},                    // not blocked
		{"user@example.com", false},
	}
	for _, c := range cases {
		got := IsRegistrationEmailSuffixBlocked(c.email, blacklist)
		if got != c.blocked {
			t.Errorf("IsRegistrationEmailSuffixBlocked(%q, %v) = %v, want %v", c.email, blacklist, got, c.blocked)
		}
	}

	// Empty blacklist blocks nothing.
	if IsRegistrationEmailSuffixBlocked("user@mailinator.com", nil) {
		t.Error("empty blacklist should block nothing")
	}
	if IsRegistrationEmailSuffixBlocked("not-an-email", blacklist) {
		t.Error("malformed email should not be blocked")
	}
}
