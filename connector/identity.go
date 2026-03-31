package connector

// Identity represents the authenticated identity that produced an item.
type Identity struct {
	AccountID  string   `json:"account_id,omitempty"`
	Provider   string   `json:"provider"`
	AuthMethod string   `json:"auth_method"`
	Scopes     []string `json:"scopes,omitempty"`
	Tenant     string   `json:"tenant,omitempty"`
}

// ConfigIdentity is the identity block from connector config.
// All fields are optional -- they provide defaults merged with per-item identity.
type ConfigIdentity struct {
	AccountID  string `json:"account_id,omitempty"`
	Provider   string `json:"provider,omitempty"`
	AuthMethod string `json:"auth_method,omitempty"`
	Tenant     string `json:"tenant,omitempty"`
}

// MergeIdentity merges config-level identity with per-item identity.
// Per-item fields override config fields. Returns nil if both are nil.
func MergeIdentity(config *ConfigIdentity, item *Identity) *Identity {
	if config == nil && item == nil {
		return nil
	}

	if config == nil {
		return copyIdentity(item)
	}

	// Start from config as base.
	merged := &Identity{
		AccountID:  config.AccountID,
		Provider:   config.Provider,
		AuthMethod: config.AuthMethod,
		Tenant:     config.Tenant,
	}

	if item == nil {
		return merged
	}

	// Per-item non-zero fields override config.
	if item.AccountID != "" {
		merged.AccountID = item.AccountID
	}
	if item.Provider != "" {
		merged.Provider = item.Provider
	}
	if item.AuthMethod != "" {
		merged.AuthMethod = item.AuthMethod
	}
	if item.Tenant != "" {
		merged.Tenant = item.Tenant
	}

	// Scopes come from item only (config has no scopes field).
	if len(item.Scopes) > 0 {
		merged.Scopes = make([]string, len(item.Scopes))
		copy(merged.Scopes, item.Scopes)
	}

	return merged
}

// copyIdentity returns a deep copy of the given Identity.
func copyIdentity(id *Identity) *Identity {
	out := &Identity{
		AccountID:  id.AccountID,
		Provider:   id.Provider,
		AuthMethod: id.AuthMethod,
		Tenant:     id.Tenant,
	}
	if len(id.Scopes) > 0 {
		out.Scopes = make([]string, len(id.Scopes))
		copy(out.Scopes, id.Scopes)
	}
	return out
}
