// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

func TestDecodeCopy(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	tests := []struct {
		in        string
		expect    string
		delimiter byte
	}{
		{
			in:     "simple",
			expect: "simple",
		},
		{
			in:     `new\nline`,
			expect: "new\nline",
		},
		{
			in:     `\b\f\n\r\t\v\\`,
			expect: "\b\f\n\r\t\v\\",
		},
		{
			in:     `\0\12\123`,
			expect: "\000\012\123",
		},
		{
			in:     `\x1\xaf`,
			expect: "\x01\xaf",
		},
		{
			in:     `T\n\07\xEV\x0fA\xb2C\1`,
			expect: "T\n\007\x0eV\x0fA\xb2C\001",
		},
		{
			in:     `\\\"`,
			expect: "\\\"",
		},
		{
			in:     `\x`,
			expect: "x",
		},
		{
			in:     `\xg`,
			expect: "xg",
		},
		{
			in:     `\`,
			expect: "\\",
		},
		{
			in:     `\8`,
			expect: "8",
		},
		{
			in:     `\a`,
			expect: "a",
		},
		{
			in:     `\x\xg\8\xH\x32\s\`,
			expect: "xxg8xH2s\\",
		},
	}

	for _, test := range tests {
		t.Run(test.in, func(t *testing.T) {
			out := DecodeCopy(test.in)
			if out != test.expect {
				t.Errorf("%q: got %q, expected %q", test.in, out, test.expect)
			}
		})
	}
}

func BenchmarkDecodeCopySimple(b *testing.B) {
	for i := 0; i < b.N; i++ {
		DecodeCopy("test string")
	}
}

func BenchmarkDecodeCopyEscaped(b *testing.B) {
	for i := 0; i < b.N; i++ {
		DecodeCopy(`string \x1 with escape`)
	}
}
