module fias-downloader

go 1.22.2

require (
	github.com/lib/pq v1.10.9
	github.com/prometheus/client_golang v1.19.1
	gopkg.in/yaml.v3 v3.0.0-00010101000000-000000000000
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/prometheus/client_model v0.5.0 // indirect
	github.com/prometheus/common v0.48.0 // indirect
	github.com/prometheus/procfs v0.12.0 // indirect
	golang.org/x/sys v0.17.0 // indirect
	google.golang.org/protobuf v1.33.0 // indirect
)

replace google.golang.org/protobuf => github.com/protocolbuffers/protobuf-go v1.33.0

replace golang.org/x/sys => github.com/golang/sys v0.17.0

replace gopkg.in/yaml.v3 => github.com/go-yaml/yaml/v3 v3.0.1

replace gopkg.in/check.v1 => github.com/go-check/check v0.0.0-20180628173108-788fd7840127
