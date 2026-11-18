// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bytes"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Faults are deliberate server bugs used to prove the suite catches them.
const (
	// FaultDigest makes the server send every module digest with its last
	// byte changed, as if it hashed module content incorrectly. Stored data
	// stays correct, so only clients that verify digests can notice.
	FaultDigest = "digest"
)

// withFault wraps the server handler to inject fault ("" for none).
func withFault(log *slog.Logger, h http.Handler, fault string) http.Handler {
	if fault == FaultDigest {
		return digestFault(log, h)
	}
	return h
}

// digestFault rewrites Connect unary protobuf responses, corrupting every
// buf.registry.*.Digest value in them.
func digestFault(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := outputType(r.URL.Path)
		if out == nil || r.Header.Get("Content-Type") != "application/proto" {
			if out != nil {
				log.Warn("digest fault not applied: unsupported content type", "path", r.URL.Path, "content_type", r.Header.Get("Content-Type"))
			}
			next.ServeHTTP(w, r)
			return
		}
		// Keep the response uncompressed so it can be rewritten.
		r.Header.Del("Accept-Encoding")
		r.Header.Del("Connect-Accept-Encoding")
		rec := &bufferingWriter{header: http.Header{}}
		next.ServeHTTP(rec, r)
		body := rec.body.Bytes()
		if rec.status == 0 || rec.status == http.StatusOK {
			msg := out.New()
			if err := proto.Unmarshal(body, msg.Interface()); err == nil {
				if n := corruptDigests(msg); n > 0 {
					if b, err := proto.Marshal(msg.Interface()); err == nil {
						body = b
						log.Warn("digest fault injected", "path", r.URL.Path, "digests", n)
					}
				}
			}
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(max(rec.status, http.StatusOK))
		_, _ = w.Write(body) //nolint:gosec // protobuf response of the test server, not HTML
	})
}

// outputType returns the response message type of the RPC at path
// (/<service>/<method>), or nil if it is not a known buf.registry RPC.
func outputType(path string) protoreflect.MessageType {
	svc, method, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !ok || !strings.HasPrefix(svc, "buf.registry.") {
		return nil
	}
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
	if err != nil {
		return nil
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil
	}
	md := sd.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		return nil
	}
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.Output().FullName())
	if err != nil {
		return nil
	}
	return mt
}

// corruptDigests flips the last byte of the value of every Digest message
// in m and returns how many it changed.
func corruptDigests(m protoreflect.Message) int {
	n := 0
	if m.Descriptor().Name() == "Digest" {
		if fd := m.Descriptor().Fields().ByName("value"); fd != nil && fd.Kind() == protoreflect.BytesKind {
			if v := bytes.Clone(m.Get(fd).Bytes()); len(v) > 0 {
				v[len(v)-1] ^= 0xff
				m.Set(fd, protoreflect.ValueOfBytes(v))
				n++
			}
		}
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Message() != nil:
			for i := range v.List().Len() {
				n += corruptDigests(v.List().Get(i).Message())
			}
		case fd.IsMap() && fd.MapValue().Message() != nil:
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				n += corruptDigests(mv.Message())
				return true
			})
		case !fd.IsList() && !fd.IsMap() && fd.Message() != nil:
			n += corruptDigests(v.Message())
		}
		return true
	})
	return n
}

type bufferingWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferingWriter) Header() http.Header         { return b.header }
func (b *bufferingWriter) WriteHeader(code int)        { b.status = code }
func (b *bufferingWriter) Write(p []byte) (int, error) { return b.body.Write(p) }
