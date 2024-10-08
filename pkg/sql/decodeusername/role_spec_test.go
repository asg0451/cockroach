// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package decodeusername

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

func TestRoleSpecValidation(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testCases := []struct {
		username   string
		normalized string
		err        string
		sqlstate   pgcode.Code
	}{
		{"Abc123", "abc123", "", pgcode.Code{}},
		{"0123121132", "0123121132", "", pgcode.Code{}},
		{"HeLlO", "hello", "", pgcode.Code{}},
		{"Ομηρος", "ομηρος", "", pgcode.Code{}},
		{"_HeLlO", "_hello", "", pgcode.Code{}},
		{"a-BC-d", "a-bc-d", "", pgcode.Code{}},
		{"A.Bcd", "a.bcd", "", pgcode.Code{}},
		{"WWW.BIGSITE.NET", "www.bigsite.net", "", pgcode.Code{}},
		{"", "", `"": username is empty`, pgcode.InvalidName},
		{"-ABC", "-abc", `"-abc": username is invalid`, pgcode.InvalidName},
		{".ABC", ".abc", `".abc": username is invalid`, pgcode.InvalidName},
		{"*.wildcard", "*.wildcard", `"\*.wildcard": username is invalid`, pgcode.InvalidName},
		{"foofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoof",
			"foofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoof",
			`"foofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoofoof": username is too long`, pgcode.NameTooLong},
		{"M", "m", "", pgcode.Code{}},
		{".", ".", `".": username is invalid`, pgcode.InvalidName},
	}

	for _, tc := range testCases {
		roleSpec := tree.RoleSpec{RoleSpecType: tree.RoleName, Name: tc.username}
		normalized, err := FromRoleSpec(
			&sessiondata.SessionData{}, username.PurposeCreation, roleSpec,
		)
		if !testutils.IsError(err, tc.err) {
			t.Errorf("%q: expected %q, got %v", tc.username, tc.err, err)
			continue
		}
		if err != nil {
			if pgcode := pgerror.GetPGCode(err); pgcode != tc.sqlstate {
				t.Errorf("%q: expected SQLSTATE %s, got %s", tc.username, tc.sqlstate, pgcode)
				continue
			}
		}
		if normalized.Normalized() != tc.normalized {
			t.Errorf("%q: expected %q, got %q", tc.username, tc.normalized, normalized.Normalized())
		}
	}
}
