module github.com/cnooljaw/gsrdemo/nodes/mole

go 1.24.0

require (
	github.com/cnooljaw/gsr v1.0.0
	github.com/cnooljaw/gsrdemo/common v0.0.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	google.golang.org/protobuf v1.36.1 // indirect
)

replace github.com/cnooljaw/gsrdemo/common => ../../common
