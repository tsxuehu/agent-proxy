module github.com/tsxuehu/agent-proxy/apps/agent-admin

go 1.25.13

require (
	github.com/redis/go-redis/v9 v9.19.0
	github.com/tsxuehu/agent-proxy/shared v0.0.0
	golang.org/x/crypto v0.46.0
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-sql-driver/mysql v1.9.3 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
)

replace github.com/tsxuehu/agent-proxy/shared => ./deps/shared
