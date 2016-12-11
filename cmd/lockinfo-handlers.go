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

package cmd

import "time"

// SystemLockState - Structure to fill the lock state of entire object storage.
// That is the total locks held, total calls blocked on locks and state of all the locks for the entire system.
type SystemLockState struct {
	TotalLocks int64 `json:"totalLocks"`
	// Count of operations which are blocked waiting for the lock to
	// be released.
	TotalBlockedLocks int64 `json:"totalBlockedLocks"`
	// Count of operations which has successfully acquired the lock but
	// hasn't unlocked yet( operation in progress).
	TotalAcquiredLocks int64            `json:"totalAcquiredLocks"`
	LocksInfoPerObject []VolumeLockInfo `json:"locksInfoPerObject"`
}

// VolumeLockInfo - Structure to contain the lock state info for volume, path pair.
type VolumeLockInfo struct {
	Bucket string `json:"bucket"`
	Object string `json:"object"`
	// All locks blocked + running for given <volume,path> pair.
	LocksOnObject int64 `json:"locksOnObject"`
	// Count of operations which has successfully acquired the lock
	// but hasn't unlocked yet( operation in progress).
	LocksAcquiredOnObject int64 `json:"locksAcquiredOnObject"`
	// Count of operations which are blocked waiting for the lock
	// to be released.
	TotalBlockedLocks int64 `json:"locksBlockedOnObject"`
	// State information containing state of the locks for all operations
	// on given <volume,path> pair.
	LockDetailsOnObject []OpsLockState `json:"lockDetailsOnObject"`
}

// OpsLockState - structure to fill in state information of the lock.
// structure to fill in status information for each operation with given operation ID.
type OpsLockState struct {
	OperationID string        `json:"opsID"`          // String containing operation ID.
	LockSource  string        `json:"lockSource"`     // Operation type (GetObject, PutObject...)
	LockType    lockType      `json:"lockType"`       // Lock type (RLock, WLock)
	Status      statusType    `json:"status"`         // Status can be Running/Ready/Blocked.
	Since       time.Time     `json:"statusSince"`    // Time when the lock was initially held.
	Duration    time.Duration `json:"statusDuration"` // Duration since the lock was held.
}

// Read entire state of the locks in the system and return.
func getSystemLockState() (SystemLockState, error) {
	globalNSMutex.lockMapMutex.Lock()
	defer globalNSMutex.lockMapMutex.Unlock()

	lockState := SystemLockState{}

	lockState.TotalBlockedLocks = globalNSMutex.blockedCounter
	lockState.TotalLocks = globalNSMutex.globalLockCounter
	lockState.TotalAcquiredLocks = globalNSMutex.runningLockCounter

	for param, debugLock := range globalNSMutex.debugLockMap {
		volLockInfo := VolumeLockInfo{}
		volLockInfo.Bucket = param.volume
		volLockInfo.Object = param.path
		volLockInfo.LocksOnObject = debugLock.ref
		volLockInfo.TotalBlockedLocks = debugLock.blocked
		volLockInfo.LocksAcquiredOnObject = debugLock.running
		for opsID, lockInfo := range debugLock.lockInfo {
			volLockInfo.LockDetailsOnObject = append(volLockInfo.LockDetailsOnObject, OpsLockState{
				OperationID: opsID,
				LockSource:  lockInfo.lockSource,
				LockType:    lockInfo.lType,
				Status:      lockInfo.status,
				Since:       lockInfo.since,
				Duration:    time.Now().UTC().Sub(lockInfo.since),
			})
		}
		lockState.LocksInfoPerObject = append(lockState.LocksInfoPerObject, volLockInfo)
	}
	return lockState, nil
}
