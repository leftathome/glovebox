package main

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connectors/powerschool"
	"github.com/leftathome/glovebox/internal/loginbroker"
	"github.com/leftathome/glovebox/internal/pslogin"
)

// driverFactory builds a site's driver from the environment. problems lists
// faults by setting name and never contains a value.
type driverFactory func(getenv func(string) string) (driver loginbroker.Driver, problems []string)

// drivers is every site this binary can serve. Adding a site is adding a
// package that implements loginbroker.Driver and one entry here; see
// internal/loginbroker/DESIGN.md.
var drivers = map[string]driverFactory{
	pslogin.SiteName: newPowerSchoolDriver,
}

func driverNames() []string {
	names := make([]string, 0, len(drivers))
	for name := range drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// newPowerSchoolDriver reads the PowerSchool settings. The portal's address
// is normalised by the connector's own function, so that the broker, the
// scheduled login job and the connector cannot disagree about where the
// portal is.
func newPowerSchoolDriver(getenv func(string) string) (loginbroker.Driver, []string) {
	var problems []string
	host := getenv("POWERSCHOOL_HOSTNAME")
	if strings.TrimSpace(host) == "" {
		host = getenv("POWERSCHOOL_HOST")
	}
	var cfg pslogin.Config
	if strings.TrimSpace(host) == "" {
		problems = append(problems, "POWERSCHOOL_HOSTNAME is not set")
	} else if base, h, err := powerschool.NormalizeBaseURL(host); err != nil {
		problems = append(problems, "POWERSCHOOL_HOSTNAME is not a host or an https URL")
	} else {
		cfg.BaseURL, cfg.Host = base, h
	}
	if b, err := strconv.ParseBool(strings.TrimSpace(getenv("POWERSCHOOL_CHROME_NO_SANDBOX"))); err == nil {
		cfg.ChromeNoSandbox = b
	}

	// In the image the temporary directory is the process's own (the chart
	// mounts a memory-backed emptyDir), so the driver may clear browser
	// leftovers out of it. The image says so; a binary run by hand on a
	// shared /tmp does not, and removes nothing.
	var opts pslogin.DriverOptions
	if b, err := strconv.ParseBool(strings.TrimSpace(getenv("LOGIN_BROKER_OWNS_TMPDIR"))); err == nil {
		opts.OwnsTempDir = b
	}
	if raw := strings.TrimSpace(getenv("POWERSCHOOL_SESSION_LIFETIME")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < time.Hour {
			problems = append(problems, "POWERSCHOOL_SESSION_LIFETIME must be a duration of at least 1h")
		} else {
			opts.SessionLifetime = d
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}
	driver, err := pslogin.NewDriver(cfg, opts)
	if err != nil {
		return nil, []string{"the PowerSchool driver could not be built"}
	}
	return driver, nil
}
