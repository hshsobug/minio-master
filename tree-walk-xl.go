/*
 * Minio Cloud Storage, (C) 2016 Minio, Inc.
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

package main

import (
	"sort"
	"strings"
	"time"
)

// listParams - list object params used for list object map
type listParams struct {
	bucket    string
	recursive bool
	marker    string
	prefix    string
}

// Tree walk result carries results of tree walking.
type treeWalkResult struct {
	entry string
	err   error
	end   bool
}

// Tree walk notify carries a channel which notifies tree walk
// results, additionally it also carries information if treeWalk
// should be timedOut.
type treeWalker struct {
	ch       <-chan treeWalkResult
	timedOut bool
}

// listDir - listDir.
func (xl xlObjects) listDir(bucket, prefixDir string, filter func(entry string) bool, isLeaf func(string, string) bool) (entries []string, err error) {
	for _, disk := range xl.getLoadBalancedQuorumDisks() {
		if disk == nil {
			continue
		}
		entries, err = disk.ListDir(bucket, prefixDir)
		if err != nil {
			break
		}
		// Skip the entries which do not match the filter.
		for i, entry := range entries {
			if filter(entry) {
				entries[i] = ""
				continue
			}
			if strings.HasSuffix(entry, slashSeparator) && isLeaf(bucket, pathJoin(prefixDir, entry)) {
				entries[i] = strings.TrimSuffix(entry, slashSeparator)
			}
		}
		sort.Strings(entries)
		// Skip the empty strings
		for len(entries) > 0 && entries[0] == "" {
			entries = entries[1:]
		}
		return entries, nil
	}

	// Return error at the end.
	return nil, err
}

// treeWalk walks directory tree recursively pushing fileInfo into the channel as and when it encounters files.
func (xl xlObjects) treeWalk(bucket, prefixDir, entryPrefixMatch, marker string, recursive bool, send func(treeWalkResult) bool, count *int, isLeaf func(string, string) bool) bool {
	// Example:
	// if prefixDir="one/two/three/" and marker="four/five.txt" treeWalk is recursively
	// called with prefixDir="one/two/three/four/" and marker="five.txt"

	var markerBase, markerDir string
	if marker != "" {
		// Ex: if marker="four/five.txt", markerDir="four/" markerBase="five.txt"
		markerSplit := strings.SplitN(marker, slashSeparator, 2)
		markerDir = markerSplit[0]
		if len(markerSplit) == 2 {
			markerDir += slashSeparator
			markerBase = markerSplit[1]
		}
	}
	entries, err := xl.listDir(bucket, prefixDir, func(entry string) bool {
		return !strings.HasPrefix(entry, entryPrefixMatch)
	}, isLeaf)
	if err != nil {
		send(treeWalkResult{err: err})
		return false
	}
	if len(entries) == 0 {
		return true
	}

	// example:
	// If markerDir="four/" Search() returns the index of "four/" in the sorted
	// entries list so we skip all the entries till "four/"
	idx := sort.Search(len(entries), func(i int) bool {
		return entries[i] >= markerDir
	})
	entries = entries[idx:]
	*count += len(entries)
	for i, entry := range entries {
		if i == 0 && markerDir == entry {
			if !recursive {
				// Skip as the marker would already be listed in the previous listing.
				*count--
				continue
			}
			if recursive && !strings.HasSuffix(entry, slashSeparator) {
				// We should not skip for recursive listing and if markerDir is a directory
				// for ex. if marker is "four/five.txt" markerDir will be "four/" which
				// should not be skipped, instead it will need to be treeWalk()'ed into.

				// Skip if it is a file though as it would be listed in previous listing.
				*count--
				continue
			}
		}

		if recursive && strings.HasSuffix(entry, slashSeparator) {
			// If the entry is a directory, we will need recurse into it.
			markerArg := ""
			if entry == markerDir {
				// We need to pass "five.txt" as marker only if we are
				// recursing into "four/"
				markerArg = markerBase
			}
			*count--
			prefixMatch := "" // Valid only for first level treeWalk and empty for subdirectories.
			if !xl.treeWalk(bucket, pathJoin(prefixDir, entry), prefixMatch, markerArg, recursive, send, count, isLeaf) {
				return false
			}
			continue
		}
		*count--
		if !send(treeWalkResult{entry: pathJoin(prefixDir, entry)}) {
			return false
		}
	}
	return true
}

// Initiate a new treeWalk in a goroutine.
func (xl xlObjects) startTreeWalk(bucket, prefix, marker string, recursive bool, isLeaf func(string, string) bool) *treeWalker {
	// Example 1
	// If prefix is "one/two/three/" and marker is "one/two/three/four/five.txt"
	// treeWalk is called with prefixDir="one/two/three/" and marker="four/five.txt"
	// and entryPrefixMatch=""

	// Example 2
	// if prefix is "one/two/th" and marker is "one/two/three/four/five.txt"
	// treeWalk is called with prefixDir="one/two/" and marker="three/four/five.txt"
	// and entryPrefixMatch="th"

	ch := make(chan treeWalkResult, maxObjectList)
	walkNotify := treeWalker{ch: ch}
	entryPrefixMatch := prefix
	prefixDir := ""
	lastIndex := strings.LastIndex(prefix, slashSeparator)
	if lastIndex != -1 {
		entryPrefixMatch = prefix[lastIndex+1:]
		prefixDir = prefix[:lastIndex+1]
	}
	count := 0
	marker = strings.TrimPrefix(marker, prefixDir)
	go func() {
		defer close(ch)
		send := func(walkResult treeWalkResult) bool {
			if count == 0 {
				walkResult.end = true
			}
			timer := time.After(time.Second * 60)
			select {
			case ch <- walkResult:
				return true
			case <-timer:
				walkNotify.timedOut = true
				return false
			}
		}
		xl.treeWalk(bucket, prefixDir, entryPrefixMatch, marker, recursive, send, &count, isLeaf)
	}()
	return &walkNotify
}

// Save the goroutine reference in the map
func (xl xlObjects) saveTreeWalk(params listParams, walker *treeWalker) {
	xl.listObjectMapMutex.Lock()
	defer xl.listObjectMapMutex.Unlock()

	walkers, _ := xl.listObjectMap[params]
	walkers = append(walkers, walker)

	xl.listObjectMap[params] = walkers
}

// Lookup the goroutine reference from map
func (xl xlObjects) lookupTreeWalk(params listParams) *treeWalker {
	xl.listObjectMapMutex.Lock()
	defer xl.listObjectMapMutex.Unlock()

	if walkChs, ok := xl.listObjectMap[params]; ok {
		for i, walkCh := range walkChs {
			if !walkCh.timedOut {
				newWalkChs := walkChs[i+1:]
				if len(newWalkChs) > 0 {
					xl.listObjectMap[params] = newWalkChs
				} else {
					delete(xl.listObjectMap, params)
				}
				return walkCh
			}
		}
		// As all channels are timed out, delete the map entry
		delete(xl.listObjectMap, params)
	}
	return nil
}
