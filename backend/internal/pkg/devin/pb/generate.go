package pb

// Protocol subset: code-yeongyu/senpi at b800427da1fb95dd18a34cd18ff1501d470356f0,
// packages/ai/proto/devin/cascade.proto. License is retained in LICENSE.senpi.
// RPC behavior was compared with WncFht/devin2api at e8547b6d60cb09fddc6debe44bafdfa69867a963.
//go:generate protoc -I . --go_out=. --go_opt=paths=source_relative cascade.proto
