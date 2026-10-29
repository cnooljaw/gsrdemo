module github.com/cnooljaw/gsrdemo/nodes/settlement

go 1.24.0

require (
	github.com/DATA-DOG/go-sqlmock v1.5.2
	github.com/cnooljaw/gsr v1.0.2
	github.com/cnooljaw/gsrdemo/common v0.0.0
	github.com/go-sql-driver/mysql v1.10.1
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/redis/go-redis/v9 v9.18.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)

replace github.com/cnooljaw/gsrdemo/common => ../../common
