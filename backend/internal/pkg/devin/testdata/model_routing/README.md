This independent schema fixture extracts the relevant declarations from the full
oh-my-pi protocol at commit `3a2758b68f7c8e6bdf22c3a76b86c8675e5e0c9e`,
`packages/ai/src/providers/devin/proto/exa/codeium_common_pb/codeium_common.proto`.
In that source, `ModelInfo.harness_uids` is a repeated string at field **20**.
The application schema is not used to generate either fixture below.

Regenerate using protoc 33.0 from this directory:

```sh
protoc -I . --descriptor_set_out=model_info.descriptor.pb upstream_model_info.proto
protoc -I . --encode=exa.codeium_common_pb.ModelInfo upstream_model_info.proto < model_info.textproto > model_info.bin
```

The decoder test checks the independent descriptor as well as the golden wire
payload. Both harness entries must survive. These fixtures establish protocol
decoding only; they provide no evidence of account availability or Fusion execution.
