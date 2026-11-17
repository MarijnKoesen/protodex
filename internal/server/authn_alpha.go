// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// `buf registry login` verifies a token by calling the legacy
// buf.alpha.registry.v1alpha1.AuthnService/GetCurrentUser (see
// docs/behavior-spec/open-questions.md, section 6). No generated code exists
// for that package, so the response message is described here by hand and
// served through dynamicpb. The request message is empty.

const authnAlphaGetCurrentUser = "/buf.alpha.registry.v1alpha1.AuthnService/GetCurrentUser"

var currentUserResponseDesc = func() protoreflect.MessageDescriptor {
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	boolean := descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	field := func(name, json string, n int32, t *descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), JsonName: proto.String(json), Number: proto.Int32(n), Type: t, Label: opt}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:       proto.String("buf/alpha/registry/v1alpha1/authn_current_user.proto"),
		Package:    proto.String("buf.alpha.registry.v1alpha1"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"google/protobuf/timestamp.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("User"), Field: []*descriptorpb.FieldDescriptorProto{
				field("id", "id", 1, str, ""),
				field("create_time", "createTime", 2, msg, ".google.protobuf.Timestamp"),
				field("update_time", "updateTime", 3, msg, ".google.protobuf.Timestamp"),
				field("username", "username", 4, str, ""),
				field("deactivated", "deactivated", 5, boolean, ""),
			}},
			{Name: proto.String("GetCurrentUserResponse"), Field: []*descriptorpb.FieldDescriptorProto{
				field("user", "user", 1, msg, ".buf.alpha.registry.v1alpha1.User"),
			}},
		},
	}, protoregistry.GlobalFiles)
	if err != nil {
		panic(err) // static descriptor; covered by tests
	}
	return fd.Messages().ByName("GetCurrentUserResponse")
}()

func authnAlphaHandler(opts ...connect.HandlerOption) (string, http.Handler) {
	h := connect.NewUnaryHandler(authnAlphaGetCurrentUser,
		func(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[dynamicpb.Message], error) {
			me, err := caller(ctx)
			if err != nil {
				return nil, err
			}
			user := dynamicpb.NewMessage(currentUserResponseDesc.Fields().ByName("user").Message())
			uf := user.Descriptor().Fields()
			user.Set(uf.ByName("id"), protoreflect.ValueOfString(me.Owner.ID))
			user.Set(uf.ByName("username"), protoreflect.ValueOfString(me.Owner.Name))
			created := timestamppb.New(me.Owner.CreatedAt).ProtoReflect()
			for _, n := range []protoreflect.Name{"create_time", "update_time"} {
				ts := dynamicpb.NewMessage(uf.ByName(n).Message())
				ts.Set(ts.Descriptor().Fields().ByName("seconds"), created.Get(created.Descriptor().Fields().ByName("seconds")))
				ts.Set(ts.Descriptor().Fields().ByName("nanos"), created.Get(created.Descriptor().Fields().ByName("nanos")))
				user.Set(uf.ByName(n), protoreflect.ValueOfMessage(ts))
			}
			resp := dynamicpb.NewMessage(currentUserResponseDesc)
			resp.Set(currentUserResponseDesc.Fields().ByName("user"), protoreflect.ValueOfMessage(user))
			return connect.NewResponse(resp), nil
		}, opts...)
	return authnAlphaGetCurrentUser, h
}
