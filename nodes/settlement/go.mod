module github.com/cnooljaw/gsrdemo/nodes/settlement

go 1.24.0

require (
	github.com/cnooljaw/gsr v0.0.0
	github.com/cnooljaw/gsrdemo/common v0.0.0
)

replace github.com/cnooljaw/gsrdemo/common => ../../common

replace github.com/cnooljaw/gsr => ../../../gsr
