package main

import "github.com/leftathome/glovebox/connector"

// Config is the unifi connector's on-disk configuration.
//
// Endpoint paths are configuration rather than constants on purpose. The UDM
// exposes several generations of API surface behind the same reverse proxy
// (the classic /proxy/network/api/s/<site>/... form and the newer integration
// routes), and which one a given firmware serves is a property of the box, not
// of this code. An operator who lands on a firmware that moved a route can
// correct it in a ConfigMap instead of waiting for a release.
type Config struct {
	// ControllerURL is the UDM base URL, e.g. "https://192.168.1.1".
	ControllerURL string `json:"controller_url"`

	// Site is the UniFi Network site identifier. Almost always "default".
	Site string `json:"site"`

	// APIKeyEnv names the environment variable holding the read-only API
	// key. The key itself is never written to config; it is projected into
	// the pod from the existing external-dns-unifi-secret.
	APIKeyEnv string `json:"api_key_env"`

	// InsecureSkipVerify disables TLS verification against the controller.
	//
	// A UDM ships a self-signed certificate for its LAN address, so this is
	// the common case on a home deployment rather than an exotic one. It is
	// still opt-in and still named honestly: turning it on means the only
	// thing authenticating the controller is the API key.
	InsecureSkipVerify bool `json:"insecure_skip_verify"`

	// CACertFile is the preferred alternative to InsecureSkipVerify: a PEM
	// bundle containing the controller's own certificate.
	CACertFile string `json:"ca_cert_file"`

	Network NetworkConfig `json:"network"`
	Protect ProtectConfig `json:"protect"`

	// WebhookSecretEnv names the environment variable holding the shared
	// secret used to verify inbound webhook pushes. When empty, the
	// listener refuses every request rather than accepting unverified ones.
	WebhookSecretEnv string `json:"webhook_secret_env"`

	// WebhookAuthMode selects how an inbound push is authenticated:
	// "hmac" verifies a signature over the body, "bearer" compares a shared
	// secret in a header.
	//
	// Both exist because UniFi's own behaviour differs by surface and by
	// firmware: Protect's alarm-manager webhooks have historically posted
	// plain JSON with no signature at all, which leaves a shared secret as
	// the only thing standing between the listener and anyone who can reach
	// the pod. Neither mode is a default-on convenience -- with no secret
	// configured the listener refuses every request.
	WebhookAuthMode string `json:"webhook_auth_mode"`

	// WebhookSignatureHeader carries the signature (hmac mode) or the shared
	// secret (bearer mode).
	WebhookSignatureHeader string `json:"webhook_signature_header"`

	ConfigIdentity *connector.ConfigIdentity `json:"config_identity,omitempty"`
}

// NetworkConfig controls the UniFi Network event surface: client
// associations, DHCP activity, and neighbouring/rogue access points.
type NetworkConfig struct {
	Enabled bool `json:"enabled"`

	// EventsPath is a printf template taking the site id.
	EventsPath string `json:"events_path"`

	// BackfillLimit bounds how many events one catch-up poll will stage.
	BackfillLimit int `json:"backfill_limit"`
}

// ProtectConfig controls the UniFi Protect event surface: motion and smart
// detections.
//
// Media (clips, audio, snapshots) is deliberately NOT fetched. Protect events
// carry a reference to their media and the reference is what reaches the
// agent; see stripMedia in event.go for why.
type ProtectConfig struct {
	Enabled bool `json:"enabled"`

	EventsPath string `json:"events_path"`

	BackfillLimit int `json:"backfill_limit"`
}

// Defaults for anything the operator left unset.
const (
	defaultSite              = "default"
	defaultNetworkEventsPath = "/proxy/network/api/s/%s/stat/event"
	defaultProtectEventsPath = "/proxy/protect/api/events"
	defaultBackfillLimit     = 200

	authModeHMAC   = "hmac"
	authModeBearer = "bearer"

	defaultSignatureHeader = "X-Unifi-Signature"
	defaultBearerHeader    = "Authorization"
)

// applyDefaults fills unset fields. It does not validate; see validate.
func (c *Config) applyDefaults() {
	if c.Site == "" {
		c.Site = defaultSite
	}
	if c.Network.EventsPath == "" {
		c.Network.EventsPath = defaultNetworkEventsPath
	}
	if c.Protect.EventsPath == "" {
		c.Protect.EventsPath = defaultProtectEventsPath
	}
	if c.Network.BackfillLimit <= 0 {
		c.Network.BackfillLimit = defaultBackfillLimit
	}
	if c.Protect.BackfillLimit <= 0 {
		c.Protect.BackfillLimit = defaultBackfillLimit
	}
	if c.APIKeyEnv == "" {
		c.APIKeyEnv = "UNIFI_API_KEY"
	}
	if c.WebhookAuthMode == "" {
		c.WebhookAuthMode = authModeHMAC
	}
	if c.WebhookSignatureHeader == "" {
		if c.WebhookAuthMode == authModeBearer {
			c.WebhookSignatureHeader = defaultBearerHeader
		} else {
			c.WebhookSignatureHeader = defaultSignatureHeader
		}
	}
}
