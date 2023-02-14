module github.com/jeevic/go-redis/example/del-keys-without-ttl

go 1.18

replace github.com/jeevic/go-redis/v9 => ../..

require (
	github.com/jeevic/go-redis/v9 v9.0.2
	go.uber.org/zap v1.24.0
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	go.uber.org/atomic v1.10.0 // indirect
	go.uber.org/multierr v1.9.0 // indirect
)
