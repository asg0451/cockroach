// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var cmds []*cobra.Command
var quiet bool

func main() {
	rootCmd := func() *cobra.Command {
		cmd := &cobra.Command{
			Use:   "docgen",
			Short: "docgen generates documentation for cockroachdb's SQL functions, grammar, and HTTP endpoints",
		}
		cmd.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "suppress output where possible")
		cmd.AddCommand(cmds...)
		return cmd
	}()
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
