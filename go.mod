module github.com/b-open-io/pay-purse

go 1.24.2

require (
	github.com/bsv-blockchain/go-sdk v1.1.22
	github.com/redis/go-redis/v9 v9.7.3
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/pkg/errors v0.9.1 // indirect
	golang.org/x/crypto v0.35.0 // indirect
)

replace github.com/bsv-blockchain/go-sdk => ../go-sdk
