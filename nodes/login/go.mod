module github.com/cnooljaw/gsrdemo/nodes/login

go 1.24.0

require (
	github.com/DATA-DOG/go-sqlmock v1.5.2
	github.com/cnooljaw/gsr v1.0.2
	github.com/cnooljaw/gsrdemo/common v0.0.0
	github.com/go-sql-driver/mysql v1.10.1
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)

replace github.com/cnooljaw/gsrdemo/common => ../../common
