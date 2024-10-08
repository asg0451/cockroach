// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package testutils

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/stretchr/testify/assert"
)

func TestScalarVars(t *testing.T) {
	var md opt.Metadata
	var sv ScalarVars

	// toStr recreates the variable definitions from md and ScalarVars.
	toStr := func() string {
		var buf bytes.Buffer
		for i := 0; i < md.NumColumns(); i++ {
			id := opt.ColumnID(i + 1)
			m := md.ColumnMeta(id)
			if i > 0 {
				buf.WriteString(", ")
			}
			fmt.Fprintf(&buf, "%s %s", m.Alias, m.Type)
			if sv.NotNullCols().Contains(id) {
				buf.WriteString(" not null")
			}
		}
		return buf.String()
	}

	vars := "a int, b string not null, c decimal"
	md.Init()
	if err := sv.Init(&md, strings.Split(vars, ", ")); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, toStr(), vars)
}
