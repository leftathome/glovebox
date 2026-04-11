# LinkedIn Connector Implementation Plan

## Overview
Implement a LinkedIn connector for the glovebox connector framework, following the GitHub connector pattern.

## Files
- [x] `connectors/linkedin/config.go` -- Config struct with BaseConfig + FeedTypes
- [x] `connectors/linkedin/connector.go` -- LinkedInConnector with Poll, fetchAPI
- [x] `connectors/linkedin/main.go` -- entrypoint with connector.Run
- [x] `connectors/linkedin/config.json` -- example config
- [x] `connectors/linkedin/Dockerfile` -- distroless build
- [x] `connectors/linkedin/connector_test.go` -- 4 TDD tests

## TDD Steps
1. [x] Write failing tests (RED) -- all 4 tests failed as expected
2. [x] Write implementation to pass tests (GREEN) -- all 4 tests pass
3. [x] Run `go test` and `go vet` to verify -- clean

## Test Cases
1. Poll fetches shares and stages them
2. Checkpoint skips duplicates
3. Identity fields in metadata (provider=linkedin, auth_method=oauth)
4. Rule tags in metadata
