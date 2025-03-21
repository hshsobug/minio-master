// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"testing"
)

// Test redactLDAPPwd()
func TestRedactLDAPPwd(t *testing.T) {
	testCases := []struct {
		query         string
		expectedQuery string
	}{
		{
			"http://152.136.119.252:9000?Action=AssumeRoleWithLDAPIdentity&LDAPUsername=hshtestnew100&LDAPPassword=hshtestnew100&Version=2011-06-15",
			"http://152.136.119.252:9000?Action=AssumeRoleWithLDAPIdentity&LDAPUsername=hshtestnew100&LDAPPassword=*REDACTED*&Version=2011-06-15",
		},
	}
	for i, test := range testCases {
		gotQuery := redactLDAPPwd(test.query)
		if gotQuery != test.expectedQuery {
			t.Fatalf("test %d: expected %s got %s", i+1, test.expectedQuery, gotQuery)
		}
	}
}
