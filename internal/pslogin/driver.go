package pslogin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/leftathome/glovebox/connectors/powerschool"
	"github.com/leftathome/glovebox/internal/dailywindow"
	"github.com/leftathome/glovebox/internal/loginbroker"
	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// SiteName is the driver's name.
const SiteName = "powerschool"

// DefaultSessionLifetime is the expires_at written into a session: login
// time plus this. An upper bound on how long the connector presents the
// session and nothing more; the portal ends an idle one in under an hour.
const DefaultSessionLifetime = 36 * time.Hour

// MinRequestInterval is the least time between two logins made because a
// connector asked: the bound on how often a connector that has been taken
// over can cause a login. The scheduled login job applies the same floor to
// its request mode.
const MinRequestInterval = 20 * time.Minute

// Driver is the PowerSchool parent portal for the login broker. It adds
// nothing to the login job's own flow; it is that flow behind the broker's
// interface:
//
//	Login   the plain-GET check that the sign-in page is the portal's own
//	        form on the configured host, then one browser login
//	Probe   the connector's own session probe: one GET of the home page
type Driver struct {
	cfg      Config
	lifetime time.Duration
	// base replaces the innermost transport of the read-only clients in
	// tests; nil is the default one.
	base http.RoundTripper
	// signIn and login are Config's production functions unless a test
	// replaced them. They are built per call so that the credentials are
	// held by nothing longer than one login.
	signIn func(cfg Config) func(ctx context.Context) error
	login  func(cfg Config) func(ctx context.Context) (*pslib.SessionExport, error)
	now    func() time.Time
	sweep  bool
}

// DriverOptions are the driver's settings beyond where the portal is.
type DriverOptions struct {
	// SessionLifetime; zero means DefaultSessionLifetime.
	SessionLifetime time.Duration
	// OwnsTempDir says the process has the temporary directory to itself
	// (a pod's own memory-backed /tmp). The driver then removes browser
	// profiles and browser scratch directories from it at startup and
	// after every login attempt. Never set it where the directory is
	// shared: it would remove another program's browser profile.
	OwnsTempDir bool
	// Base, Login and Now are for tests.
	Base  http.RoundTripper
	Login func(cfg Config) func(ctx context.Context) (*pslib.SessionExport, error)
	Now   func() time.Time
}

// NewDriver builds the driver for the portal at cfg.BaseURL / cfg.Host.
// cfg.Username and cfg.Password are ignored: the broker hands the
// credentials to Login.
func NewDriver(cfg Config, opts DriverOptions) (*Driver, error) {
	if cfg.BaseURL == "" || cfg.Host == "" {
		return nil, errors.New("pslogin: the portal's address is not set")
	}
	cfg.Username, cfg.Password = "", ""
	d := &Driver{
		cfg:      cfg,
		lifetime: opts.SessionLifetime,
		base:     opts.Base,
		login:    opts.Login,
		now:      opts.Now,
	}
	if d.lifetime == 0 {
		d.lifetime = DefaultSessionLifetime
	}
	if d.lifetime < time.Hour {
		return nil, errors.New("pslogin: the session lifetime must be at least 1h")
	}
	d.signIn = func(c Config) func(ctx context.Context) error { return SignInCheck(c, d.base) }
	if d.login == nil {
		d.login = Login
	}
	if d.now == nil {
		d.now = time.Now
	}
	d.sweep = opts.OwnsTempDir
	if d.sweep {
		// Whatever an earlier process left behind -- one that was killed
		// in the middle of a login -- goes before anything else is done.
		SweepBrowserLeftovers()
	}
	return d, nil
}

var (
	_ loginbroker.Driver      = (*Driver)(nil)
	_ loginbroker.Constrained = (*Driver)(nil)
)

// Site implements loginbroker.Driver.
func (d *Driver) Site() string { return SiteName }

// Constraints implements loginbroker.Constrained: the account's owner uses
// the portal between 16:00 and 21:00 Pacific and a login then signs them
// out (psportal's floor). Configuration can add windows; it cannot remove
// this one, shorten it or move it to another zone.
func (d *Driver) Constraints() loginbroker.Constraints {
	start, _ := dailywindow.ParseClock(psportal.FloorStart)
	end, _ := dailywindow.ParseClock(psportal.FloorEnd)
	return loginbroker.Constraints{
		Blackout: dailywindow.Schedule{{
			Windows: []dailywindow.Window{{Start: start, End: end}},
			Loc:     psportal.FloorLocation(),
		}},
		MinInterval:   MinRequestInterval,
		LoginHeadroom: LoginHeadroom,
		Location:      psportal.FloorLocation(),
	}
}

// LoginHeadroom is how far ahead of one call of Driver.Login the blackout
// must be clear: everything the call does before the credentials are
// submitted, plus a margin. That is the plain-GET check of the sign-in page
// (up to ProbeTimeout) AND the browser login (up to LoginTimeout) --
// psportal.LoginHeadroom alone covers only the second, which is right for
// the scheduled job (it checks the blackout between the two) and too short
// here. The last instant a broker login may start before 16:00 is 15:56:59.
const LoginHeadroom = ProbeTimeout + psportal.LoginHeadroom

// Login implements loginbroker.Driver. The credentials are submitted by the
// browser, at most once: the library types them into the sign-in form and
// submits it one time, and nothing here calls it twice.
func (d *Driver) Login(ctx context.Context, creds loginbroker.Credentials) (loginbroker.Session, error) {
	// A browser profile holds the session cookies. The login removes its
	// own on every way out; this also removes what a browser that crashed
	// or was cancelled left beside it.
	if d.sweep {
		defer SweepBrowserLeftovers()
	}
	// Is the page the browser is about to type the password into the
	// portal's own sign-in form? A plain GET, before any browser exists.
	// This check needs no credentials and is not given any.
	if err := d.signIn(d.cfg)(ctx); err != nil {
		if errors.Is(err, ErrSignInUnrecognised) {
			return nil, &loginbroker.LoginError{
				Kind: loginbroker.LoginNotSubmitted, Detail: "signin_page_unrecognised:" + SignInReason(err), Err: err}
		}
		return nil, &loginbroker.LoginError{
			Kind: loginbroker.LoginNotSubmitted, Detail: "signin_page_unreachable:" + errorClass(err), Err: err}
	}

	cfg := d.cfg
	cfg.Username, cfg.Password = creds.Username, creds.Password
	session, err := d.login(cfg)(ctx)
	if err != nil {
		switch KindOf(err) {
		case Rejected:
			return nil, &loginbroker.LoginError{Kind: loginbroker.LoginRejected, Detail: "credentials_rejected", Err: err}
		case FailedBeforeSubmit:
			return nil, &loginbroker.LoginError{Kind: loginbroker.LoginNotSubmitted, Detail: "browser_or_network_before_submit", Err: err}
		}
		return nil, &loginbroker.LoginError{Kind: loginbroker.LoginUnknown, Detail: "after_or_during_submit", Err: err}
	}
	if session == nil {
		return nil, &loginbroker.LoginError{Kind: loginbroker.LoginUnknown, Detail: "no_session"}
	}
	normal := NormalizeSession(session, d.now().Add(d.lifetime))
	if normal == nil {
		return nil, &loginbroker.LoginError{Kind: loginbroker.LoginUnknown, Detail: "no_usable_cookies"}
	}
	return normal, nil
}

// errorClass is the connector's classification of an error
// (powerschool.ErrorAttrs' error_class), or "error".
func errorClass(err error) string {
	attrs := powerschool.ErrorAttrs(err)
	for i := 0; i+1 < len(attrs); i += 2 {
		if k, ok := attrs[i].(string); ok && k == "error_class" {
			if v, ok := attrs[i+1].(string); ok && v != "" {
				return v
			}
		}
	}
	return "error"
}

// Probe implements loginbroker.Driver with the connector's own probe, so
// the broker and the connector cannot disagree about whether a session is
// dead. One GET, through a transport that can only GET.
func (d *Driver) Probe(ctx context.Context, session loginbroker.Session) loginbroker.ProbeResult {
	s, ok := session.(*pslib.SessionExport)
	if !ok || s == nil {
		return loginbroker.ProbeResult{State: loginbroker.ProbeDead, Reason: powerschool.ReasonNoSession}
	}
	if !s.ExpiresAt.After(d.now()) {
		// The connector would refuse to present it; so does this.
		return loginbroker.ProbeResult{State: loginbroker.ProbeDead, Reason: powerschool.ReasonExpiredDocument}
	}
	verdict := Verdict(ctx, d.cfg, d.base, s)
	switch verdict.State {
	case powerschool.SessionLive:
		return loginbroker.ProbeResult{State: loginbroker.ProbeAlive, Reason: verdict.Reason}
	case powerschool.SessionDead:
		return loginbroker.ProbeResult{State: loginbroker.ProbeDead, Reason: verdict.Reason}
	}
	return loginbroker.ProbeResult{State: loginbroker.ProbeUnknown, Reason: verdict.Reason}
}

// Encode implements loginbroker.Driver: the same secret the scheduled login
// job writes (SessionData). The selected student is not recorded by the
// broker; the field is written empty so the secret keeps its shape.
func (d *Driver) Encode(session loginbroker.Session, loggedInAt time.Time) (map[string]string, error) {
	s, ok := session.(*pslib.SessionExport)
	if !ok || s == nil {
		return nil, errors.New("pslogin: not a PowerSchool session")
	}
	data, err := SessionData(s, loggedInAt, "")
	if err != nil {
		return nil, errors.New("pslogin: the session could not be encoded")
	}
	out := make(map[string]string, len(data))
	for k, v := range data {
		if str, ok := v.(string); ok {
			out[k] = str
		}
	}
	if loggedInAt.IsZero() {
		delete(out, FieldLoggedInAt)
	}
	return out, nil
}

// Decode implements loginbroker.Driver. A document past its expires_at is
// still decoded: Probe reports it dead, which is what leads to a login.
func (d *Driver) Decode(fields map[string]string) (loginbroker.Session, error) {
	doc := fields[FieldSession]
	if doc == "" {
		return nil, errors.New("pslogin: the stored secret has no session field")
	}
	// A far-past "now" makes ParseSession check everything but the expiry.
	s, err := powerschool.ParseSession([]byte(doc), time.Time{})
	if err != nil {
		// ParseSession never quotes the document.
		return nil, err
	}
	return s, nil
}

// Fingerprint implements loginbroker.Driver with the connector's own
// function.
func (d *Driver) Fingerprint(session loginbroker.Session) string {
	s, ok := session.(*pslib.SessionExport)
	if !ok {
		return ""
	}
	return powerschool.SessionFingerprint(s)
}
