// SPDX-License-Identifier: Apache-2.0

// Command protoc-gen-conformance is a minimal protoc plugin used by the
// conformance suite's `buf generate` scenario. For every file to generate it
// writes <file>.txt listing the file's package, messages and imports, so the
// test can check which files (local, remote, imports) reached the plugin.
// The plugin parameter, if any, is written on the first line.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "protoc-gen-conformance:", err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer) error {
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	var req pluginpb.CodeGeneratorRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return err
	}
	byName := map[string]*descriptorpb.FileDescriptorProto{}
	for _, f := range req.GetProtoFile() {
		byName[f.GetName()] = f
	}
	resp := &pluginpb.CodeGeneratorResponse{}
	for _, name := range req.GetFileToGenerate() {
		f := byName[name]
		if f == nil {
			return fmt.Errorf("file to generate %q not in request", name)
		}
		var b strings.Builder
		if p := req.GetParameter(); p != "" {
			fmt.Fprintf(&b, "parameter: %s\n", p)
		}
		fmt.Fprintf(&b, "file: %s\npackage: %s\n", f.GetName(), f.GetPackage())
		for _, m := range f.GetMessageType() {
			fmt.Fprintf(&b, "message: %s\n", m.GetName())
		}
		for _, d := range f.GetDependency() {
			fmt.Fprintf(&b, "import: %s\n", d)
		}
		resp.File = append(resp.File, &pluginpb.CodeGeneratorResponse_File{
			Name:    proto.String(name + ".txt"),
			Content: proto.String(b.String()),
		})
	}
	data, err = proto.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}
