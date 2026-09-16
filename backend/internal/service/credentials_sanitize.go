package service

// SanitizeStoredCredentials strips secrets that must never be persisted on the
// account credentials map after conversion to OAuth tokens (Grok Web SSO / password).
// Call from admin create/update/import/apply-oauth paths.
//
// Cookie is stripped for API/OAuth platforms: bulk paths may pass an empty
// platform label, and session-jar residue must never sit next to OAuth tokens
// on any platform. Exception: web reverse platforms (web-deepseek/web-zhipu,
// per IsWebProvider) hold their login state in the cookie itself — it IS the
// credential, encrypted at rest with the rest of the credentials map. Empty
// platform labels still strip (web accounts only enter with an explicit
// platform label from the admin create/update paths).
func SanitizeStoredCredentials(platform string, creds map[string]any) map[string]any {
	if creds == nil {
		return nil
	}
	keys := []string{"password", "sso_token", "sso", "sso-rw", "clearTextPassword", "cookie"}
	if IsWebProvider(platform) {
		keys = []string{"password", "sso_token", "sso", "sso-rw", "clearTextPassword"}
	}
	for _, key := range keys {
		delete(creds, key)
	}
	return creds
}
