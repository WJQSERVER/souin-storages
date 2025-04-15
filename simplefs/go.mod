module github.com/WJQSERVER/souin-storages/simplefs

go 1.24

replace github.com/darkweak/storages/core => ../core

require (
	github.com/darkweak/storages/core v0.0.14
	github.com/dustin/go-humanize v1.0.1
	github.com/jellydator/ttlcache/v3 v3.3.0
	github.com/pierrec/lz4/v4 v4.1.22
)

require (
	golang.org/x/sync v0.13.0 // indirect
	google.golang.org/protobuf v1.36.6 // indirect
)


