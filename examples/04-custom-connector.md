# Building a Custom Connector

*2026-03-31T07:17:27Z by Showboat 0.6.1*
<!-- showboat-id: 986f63b3-df5c-4705-a364-7384c12b5ba7 -->

This demo walks through creating a custom connector using the scaffold generator, developing it as a standalone project, and then contributing it back as a pull request.

## Part 1: Generate the scaffold

The scaffold generator creates a ready-to-customize connector:

```bash
go run ./generator new-connector weather-alerts 2>&1; echo 'generated'; ls connectors/weather-alerts/
```

```output
connector scaffolded at /mnt/c/Users/steve/Code/glovebox/connectors/weather-alerts
generated
Dockerfile
README.md
config.go
config.json
connector.go
main.go
```

The generated connector compiles immediately:

```bash
go build ./connectors/weather-alerts/ && echo 'builds successfully'
```

```output
found packages weatheralerts (config.go) and main (main.go) in /mnt/c/Users/steve/Code/glovebox/connectors/weather-alerts
```

Inspect the generated Poll stub:

```bash
cat connectors/weather-alerts/connector.go
```

```output
package weatheralerts

import (
	"context"

	"github.com/leftathome/glovebox/connector"
)

// WeatherAlertsConnector implements the connector.Connector interface.
type WeatherAlertsConnector struct {
	config Config
	writer *connector.StagingWriter
	matcher *connector.RuleMatcher
}

// Poll fetches data from the source and writes it to staging.
func (c *WeatherAlertsConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
	// TODO: implement fetch logic
	return nil
}
```

Inspect the generated main.go (already wired to the connector framework):

```bash
cat connectors/weather-alerts/main.go
```

```output
package main

import (
	"os"
	"time"

	"github.com/leftathome/glovebox/connector"
)

func main() {
	c := &WeatherAlertsConnector{}
	connector.Run(connector.Options{
		Name:       "weather-alerts",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
		ConfigFile: os.Getenv("GLOVEBOX_CONNECTOR_CONFIG"),
		Connector:  c,
		Setup: func(cc connector.ConnectorContext) error {
			c.writer = cc.Writer
			c.matcher = cc.Matcher
			return nil
		},
		PollInterval: 5 * time.Minute,
	})
}
```

The generated config uses the unified rules format:

```bash
cat connectors/weather-alerts/config.json
```

```output
{
    "rules": [
        {"match": "*", "destination": "default"}
    ]
}
```

## Part 2: Maintaining as a standalone project

To develop this connector outside the glovebox monorepo, copy the generated directory to its own repository. The connector imports `github.com/leftathome/glovebox/connector` as a Go module dependency -- it does not need the rest of the glovebox codebase.

```
# In your own repo:
mkdir my-weather-connector && cd my-weather-connector
cp -r /path/to/glovebox/connectors/weather-alerts/* .
go mod init github.com/myorg/weather-connector
go mod edit -require github.com/leftathome/glovebox@v0.2.0
go mod tidy
go build .
```

## Part 3: Contributing back as a pull request

To contribute your connector to the glovebox project:

```
# Fork and clone the glovebox repo
gh repo fork leftathome/glovebox --clone
cd glovebox

# Generate the scaffold (or copy your existing connector)
go run ./generator new-connector weather-alerts

# Implement your connector, add tests
# ... edit connectors/weather-alerts/connector.go ...
# ... create connectors/weather-alerts/connector_test.go ...

# Verify everything passes
go vet ./connectors/weather-alerts/...
go test ./connectors/weather-alerts/...

# Commit and open a PR
git checkout -b feat/weather-alerts-connector
git add connectors/weather-alerts/
git commit -m "feat: add weather alerts connector"
gh pr create --title "feat: weather alerts connector" \
  --body "Adds a connector for weather alert feeds."
```

Key requirements for contributed connectors:

- Implement the `connector.Connector` interface (Poll method at minimum)
- Use the unified rules config (`rules` not `routes`)
- Set `Identity` on all staged items
- Pass `RuleTags` from `MatchResult` to `ItemOptions`
- Include tests using `httptest` mock servers
- Include a Dockerfile following the distroless pattern
- No external dependencies beyond the Go standard library and `connector/`
- Run `go vet` and `go test` cleanly

Clean up the generated scaffold:

```bash
rm -rf connectors/weather-alerts && echo 'cleaned up'
```

```output
cleaned up
```
