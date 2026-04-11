# OneDrive Connector Implementation Plan

## Overview
Implement a OneDrive activity connector via Microsoft Graph delta API, following the gdrive connector pattern.

## Files to Create
- [x] connectors/onedrive/connector_test.go -- tests first (red)
- [ ] connectors/onedrive/config.go
- [ ] connectors/onedrive/connector.go
- [ ] connectors/onedrive/main.go
- [ ] connectors/onedrive/config.json
- [ ] connectors/onedrive/Dockerfile

## Tests
1. Poll fetches changes and stages items
2. Checkpoint uses deltaLink
3. Initial delta fetch (no checkpoint)
4. Identity fields in metadata (provider=microsoft, auth_method=oauth)
5. Rule tags in metadata

## Key Differences from gdrive
- Uses Microsoft Graph delta API instead of Google Drive Changes API
- Delta pattern: initial GET /me/drive/root/delta returns changes + @odata.deltaLink
- Subsequent runs use the stored deltaLink URL directly
- No separate startPageToken endpoint -- initial call to delta returns both changes and deltaLink
- Auth env vars: MS_CLIENT_ID, MS_CLIENT_SECRET, MS_TENANT_ID
- Identity: provider=microsoft
- Route: drive:changes (same route key)

## Progress
- Starting TDD cycle
