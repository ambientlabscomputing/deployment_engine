module github.com/ambientlabscomputing/deployment_engine

go 1.24.0

require (
	github.com/ambientlabscomputing/umc_sdk v0.1.1
	google.golang.org/grpc v1.78.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/lmittmann/tint v1.0.6 // indirect
	github.com/natefinch/lumberjack v2.0.0+incompatible // indirect
	golang.org/x/net v0.47.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/text v0.31.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251029180050-ab9386a59fda // indirect
)

replace github.com/ambientlabscomputing/umc_sdk => ../umc_sdk
