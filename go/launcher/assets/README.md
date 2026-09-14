# assets

`scripts/build-helper.sh` copies `piko-expose-<os>-<arch>` in here before it
builds the launcher, so the launcher can carry the tunnel engine for its own
platform and plant it into a plugin installed from GitHub or npm (where `bin/`
is absent, because binaries are build output).

This file is what keeps `//go:embed assets` valid when the directory is empty:
`go build ./...` on a fresh checkout still compiles.
