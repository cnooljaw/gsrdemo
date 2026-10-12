module github.com/cnooljaw/gsrdemo/nodes/access

go 1.24.0

require (
	github.com/cnooljaw/gsr v0.0.0
	github.com/cnooljaw/gsrdemo/common v0.0.0
	google.golang.org/protobuf v1.36.1
)

require github.com/gorilla/websocket v1.5.3 // indirect

replace github.com/cnooljaw/gsrdemo/common => ../../common

replace github.com/cnooljaw/gsr => ../../../gsr
