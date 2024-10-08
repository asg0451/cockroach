// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// whereis is a helper executable that is basically just `realpath`. It's meant
// to be used like:
//     bazel run ... --run_under //build/bazelutil/whereis
// ... which will print the location of the binary you're running. Useful
// because Bazel can be a little unclear about where exactly to find any given
// executable.
func main() {
	if len(os.Args) != 2 {
		panic("expected a single argument")
	}
	abs, err := filepath.Abs(os.Args[1])
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", abs)
}
