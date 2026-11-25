module github.com/tsxuehu/agent-proxy/tests/integration

go 1.25.13

require github.com/tsxuehu/agent-proxy/shared v0.0.0

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/go-sql-driver/mysql v1.9.3 // indirect
	github.com/gorilla/websocket v1.5.3
)

replace github.com/tsxuehu/agent-proxy/shared => ../../shared
