// Separate module so the parent's `go test ./...` / `go vet ./...` skip the
// intentionally-broken fixtures under tasks/. Never built or imported directly;
// fixtures are copied into a scratch working copy at eval time.
module octo-agent-evals

go 1.22
