## Change

Describe the Source Spec, importer, overlay, fixture, runtime, or documentation change and why it is needed.

## Source checklist

For non-source changes, mark source-only items as not applicable.

- [ ] The source ID, upstream URL, version, ecosystem, and license are accurate.
- [ ] I have the right to redistribute the submitted rule and fixture content.
- [ ] No token, authorization header, personal Cookie, signed media URL, or user identifier is committed.
- [ ] `allowed_hosts`, timeouts, redirects, response size, concurrency, and request rate use the smallest practical scope.
- [ ] Search, exact episode matching, no-match/failure behavior, and every transport path have offline coverage.
- [ ] The fixture is minimal, redacted, stable, and permitted for redistribution.
- [ ] I checked for an existing Animeko, Kazumi, Nagare v1, or community source for the same site.
- [ ] `go run ./cmd/nagare-source validate` passes.
- [ ] `go test ./...` passes.
- [ ] Reproducible builds or generated reports affected by this change were verified.
