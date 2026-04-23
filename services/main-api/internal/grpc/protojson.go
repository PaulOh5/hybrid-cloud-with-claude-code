package grpcsrv

import (
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// protojsonMarshal uses protojson with stable field names (no RandomizeFieldOrder)
// so equivalent topologies yield byte-identical JSON for cache/hash purposes.
var protojsonOpts = protojson.MarshalOptions{
	UseProtoNames:   true,
	EmitUnpopulated: false,
}

func protojsonMarshal(m proto.Message) ([]byte, error) {
	return protojsonOpts.Marshal(m)
}
