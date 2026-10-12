module github.com/orpheus-agents/orpheus-mattermost

go 1.27.0

require (
	github.com/abox-dev/sdk/packages/go-sdk v0.2.0
	github.com/coder/websocket v1.8.15
	github.com/google/uuid v1.6.0
	github.com/orpheus-agents/orpheus v0.6.0
	github.com/prometheus/client_golang v1.24.1
	github.com/yuin/goldmark v1.8.6
	gopkg.in/yaml.v3 v3.0.1
)

require (
	connectrpc.com/connect v1.19.1 // indirect
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/telemetry v0.0.0-20260811182544-a038080d80e5 // indirect
	golang.org/x/tools v0.49.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

tool golang.org/x/tools/cmd/deadcode

// Temporary pin until the service-credentials core PR is released.
replace github.com/orpheus-agents/orpheus => github.com/borozdmu/orpheus v0.0.0-20261012155327-c687700e3295
