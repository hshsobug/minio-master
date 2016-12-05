/*
 * Minio Cloud Storage (C) 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bytes"
	"strconv"
	"testing"
)

// TestListObjectsHeal - Tests ListObjectsHeal API for XL
func TestListObjectsHeal(t *testing.T) {

	rootPath, err := newTestConfig("us-east-1")
	if err != nil {
		t.Fatalf("Init Test config failed")
	}
	// remove the root directory after the test ends.
	defer removeAll(rootPath)

	// Create an instance of xl backend
	xl, fsDirs, err := prepareXL()
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup backend directories
	defer removeRoots(fsDirs)

	bucketName := "bucket"
	objName := "obj"

	// Create test bucket
	err = xl.MakeBucket(bucketName)
	if err != nil {
		t.Fatal(err)
	}

	// Put 500 objects under sane dir
	for i := 0; i < 500; i++ {
		_, err = xl.PutObject(bucketName, "sane/"+objName+strconv.Itoa(i), int64(len("abcd")), bytes.NewReader([]byte("abcd")), nil, "")
		if err != nil {
			t.Fatalf("XL Object upload failed: <ERROR> %s", err)
		}
	}
	// Put 500 objects under unsane/subdir dir
	for i := 0; i < 500; i++ {
		_, err = xl.PutObject(bucketName, "unsane/subdir/"+objName+strconv.Itoa(i), int64(len("abcd")), bytes.NewReader([]byte("abcd")), nil, "")
		if err != nil {
			t.Fatalf("XL Object upload failed: <ERROR> %s", err)
		}
	}

	// Structure for testing
	type testData struct {
		bucket      string
		object      string
		marker      string
		delimiter   string
		maxKeys     int
		expectedErr error
		foundObjs   int
	}

	// Generic function for testing ListObjectsHeal, needs testData as a parameter
	testFunc := func(testCase testData, testRank int) {
		objectsNeedHeal, foundErr := xl.ListObjectsHeal(testCase.bucket, testCase.object, testCase.marker, testCase.delimiter, testCase.maxKeys)
		if testCase.expectedErr == nil && foundErr != nil {
			t.Fatalf("Test %d: Expected nil error, found: %v", testRank, foundErr)
		}
		if testCase.expectedErr != nil && foundErr.Error() != testCase.expectedErr.Error() {
			t.Fatalf("Test %d: Found unexpected error: %v, expected: %v", testRank, foundErr, testCase.expectedErr)

		}
		if len(objectsNeedHeal.Objects) != testCase.foundObjs {
			t.Fatalf("Test %d: Found unexpected number of objects: %d, expected: %v", testRank, len(objectsNeedHeal.Objects), testCase.foundObjs)
		}
	}

	// Start tests

	testCases := []testData{
		// Wrong bucket name
		{"foobucket", "", "", "", 1000, BucketNotFound{Bucket: "foobucket"}, 0},
		// Inexistent object
		{bucketName, "inexistentObj", "", "", 1000, nil, 0},
		// Test ListObjectsHeal when all objects are sane
		{bucketName, "", "", "", 1000, nil, 0},
	}
	for i, testCase := range testCases {
		testFunc(testCase, i+1)
	}

	// Test ListObjectsHeal when all objects under unsane need healing
	xlObj := xl.(*xlObjects)
	for i := 0; i < 500; i++ {
		if err = xlObj.storageDisks[0].DeleteFile(bucketName, "unsane/subdir/"+objName+strconv.Itoa(i)+"/xl.json"); err != nil {
			t.Fatal(err)
		}
	}

	// Start tests again with some objects that need healing

	testCases = []testData{
		// Test ListObjectsHeal when all objects under unsane/ need to be healed
		{bucketName, "", "", "", 1000, nil, 500},
		// List objects heal under unsane/, should return all elements
		{bucketName, "unsane/", "", "", 1000, nil, 500},
		// List healing objects under sane/, should return 0
		{bucketName, "sane/", "", "", 1000, nil, 0},
		// Max Keys == 200
		{bucketName, "unsane/", "", "", 200, nil, 200},
		// Max key > 1000
		{bucketName, "unsane/", "", "", 5000, nil, 500},
		// Prefix == Delimiter == "/"
		{bucketName, "/", "", "/", 5000, nil, 0},
		// Max Keys == 0
		{bucketName, "", "", "", 0, nil, 0},
		// Testing with marker parameter
		{bucketName, "", "unsane/subdir/" + objName + "0", "", 1000, nil, 499},
	}
	for i, testCase := range testCases {
		testFunc(testCase, i+1)
	}

}
