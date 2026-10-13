module github.com/cnooljaw/gsrdemo/nodes/login

go 1.24.0

require (
	github.com/cnooljaw/gsr v1.0.0
	github.com/cnooljaw/gsrdemo/common v0.0.0
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/crypto v0.26.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/sys v0.30.0 // indirect
	google.golang.org/protobuf v1.36.1 // indirect
)

replace github.com/cnooljaw/gsrdemo/common => ../../common
