# Teams Messages Connector Implementation Plan

## Overview
Microsoft Teams messages connector via Microsoft Graph API, following the same patterns as the GitHub connector.

## Files to Create
- [x] `connectors/teams/config.go` - Config and ChannelConfig types
- [x] `connectors/teams/connector.go` - TeamsConnector with Poll method
- [x] `connectors/teams/connector_test.go` - Tests (mock HTTP server)
- [x] `connectors/teams/main.go` - Entry point
- [x] `connectors/teams/config.json` - Example config
- [x] `connectors/teams/Dockerfile` - Container build

## Status
- [ ] Write failing tests (RED)
- [ ] Implement connector (GREEN)
- [ ] Run go test and go vet
