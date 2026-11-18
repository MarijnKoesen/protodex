// SPDX-License-Identifier: Apache-2.0

// Command app builds a message that uses a type from the hello module and
// prints it as JSON. Run `buf generate` first; see ../README.md.
package main

import (
	"fmt"
	"log"

	"google.golang.org/protobuf/encoding/protojson"

	appv1 "example.com/app/gen/app/v1"
	hellov1 "example.com/app/gen/hello/v1"
)

func main() {
	req := &appv1.Request{Ping: &hellov1.Ping{Message: "hello from protodex"}}
	b, err := protojson.Marshal(req)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(b))
}
