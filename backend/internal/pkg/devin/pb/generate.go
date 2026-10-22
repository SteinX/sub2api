package pb

// Protocol subset: code-yeongyu/senpi at b800427da1fb95dd18a34cd18ff1501d470356f0,
// packages/ai/proto/devin/cascade.proto. License is retained in LICENSE.senpi.
// RPC behavior was compared with WncFht/devin2api at e8547b6d60cb09fddc6debe44bafdfa69867a963.
// ModelInfo.harness_uids: repeated string, field 20, independently checked against
// can1357/oh-my-pi at 3a2758b68f7c8e6bdf22c3a76b86c8675e5e0c9e,
// packages/ai/src/providers/devin/proto/exa/codeium_common_pb/codeium_common.proto.
// ModelOrAlias and chat fields were checked against can1357/oh-my-pi at
// 0b5c27ab24719c86a5986b95ee55f4e75bc1aaa0, packages/catalog/src/discovery/devin-proto.ts.
//go:generate protoc -I . --go_out=. --go_opt=paths=source_relative cascade.proto
