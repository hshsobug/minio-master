/*
 * MinIO Cloud Storage, (C) 2018 MinIO, Inc.
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
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"sync"
	"time"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/minio/minio-go/v6/pkg/set"
	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/auth"
	iampolicy "github.com/minio/minio/pkg/iam/policy"
	"github.com/minio/minio/pkg/madmin"
)

const (
	// IAM configuration directory.
	iamConfigPrefix = minioConfigPrefix + "/iam"

	// IAM users directory.
	iamConfigUsersPrefix = iamConfigPrefix + "/users/"

	// IAM groups directory.
	iamConfigGroupsPrefix = iamConfigPrefix + "/groups/"

	// IAM policies directory.
	iamConfigPoliciesPrefix = iamConfigPrefix + "/policies/"

	// IAM sts directory.
	iamConfigSTSPrefix = iamConfigPrefix + "/sts/"

	// IAM Policy DB prefixes.
	iamConfigPolicyDBPrefix         = iamConfigPrefix + "/policydb/"
	iamConfigPolicyDBUsersPrefix    = iamConfigPolicyDBPrefix + "users/"
	iamConfigPolicyDBSTSUsersPrefix = iamConfigPolicyDBPrefix + "sts-users/"
	iamConfigPolicyDBGroupsPrefix   = iamConfigPolicyDBPrefix + "groups/"

	// IAM identity file which captures identity credentials.
	iamIdentityFile = "identity.json"

	// IAM policy file which provides policies for each users.
	iamPolicyFile = "policy.json"

	// IAM group members file
	iamGroupMembersFile = "members.json"

	// IAM format file
	iamFormatFile = "format.json"

	iamFormatVersion1 = 1
)

const (
	statusEnabled  = "enabled"
	statusDisabled = "disabled"
)

type iamFormat struct {
	Version int `json:"version"`
}

func getCurrentIAMFormat() iamFormat {
	return iamFormat{Version: iamFormatVersion1}
}

func getIAMFormatFilePath() string {
	return iamConfigPrefix + "/" + iamFormatFile
}

func getUserIdentityPath(user string, isSTS bool) string {
	basePath := iamConfigUsersPrefix
	if isSTS {
		basePath = iamConfigSTSPrefix
	}
	return pathJoin(basePath, user, iamIdentityFile)
}

func getGroupInfoPath(group string) string {
	return pathJoin(iamConfigGroupsPrefix, group, iamGroupMembersFile)
}

func getPolicyDocPath(name string) string {
	return pathJoin(iamConfigPoliciesPrefix, name, iamPolicyFile)
}

func getMappedPolicyPath(name string, isSTS, isGroup bool) string {
	switch {
	case isSTS:
		return pathJoin(iamConfigPolicyDBSTSUsersPrefix, name+".json")
	case isGroup:
		return pathJoin(iamConfigPolicyDBGroupsPrefix, name+".json")
	default:
		return pathJoin(iamConfigPolicyDBUsersPrefix, name+".json")
	}
}

// UserIdentity represents a user's secret key and their status
type UserIdentity struct {
	Version     int              `json:"version"`
	Credentials auth.Credentials `json:"credentials"`
}

func newUserIdentity(creds auth.Credentials) UserIdentity {
	return UserIdentity{Version: 1, Credentials: creds}
}

// GroupInfo contains info about a group
type GroupInfo struct {
	Version int      `json:"version"`
	Status  string   `json:"status"`
	Members []string `json:"members"`
}

func newGroupInfo(members []string) GroupInfo {
	return GroupInfo{Version: 1, Status: statusEnabled, Members: members}
}

// MappedPolicy represents a policy name mapped to a user or group
type MappedPolicy struct {
	Version int    `json:"version"`
	Policy  string `json:"policy"`
}

func newMappedPolicy(policy string) MappedPolicy {
	return MappedPolicy{Version: 1, Policy: policy}
}

func loadIAMConfigItem(objectAPI ObjectLayer, item interface{}, path string) error {
	data, err := readConfig(context.Background(), objectAPI, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, item)
}

func saveIAMConfigItem(objectAPI ObjectLayer, item interface{}, path string) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	return saveConfig(context.Background(), objectAPI, path, data)
}

// helper type for listIAMConfigItems
type itemOrErr struct {
	Item string
	Err  error
}

// Lists files or dirs in the minioMetaBucket at the given path
// prefix. If dirs is true, only directories are listed, otherwise
// only objects are listed. All returned items have the pathPrefix
// removed from their names.
func listIAMConfigItems(objectAPI ObjectLayer, pathPrefix string, dirs bool,
	doneCh <-chan struct{}) <-chan itemOrErr {

	ch := make(chan itemOrErr)
	dirList := func(lo ListObjectsInfo) []string {
		return lo.Prefixes
	}
	filesList := func(lo ListObjectsInfo) (r []string) {
		for _, o := range lo.Objects {
			r = append(r, o.Name)
		}
		return r
	}

	go func() {
		marker := ""
		for {
			lo, err := objectAPI.ListObjects(context.Background(),
				minioMetaBucket, pathPrefix, marker, "/", 1000)
			if err != nil {
				select {
				case ch <- itemOrErr{Err: err}:
				case <-doneCh:
				}
				close(ch)
				return
			}
			marker = lo.NextMarker
			lister := dirList(lo)
			if !dirs {
				lister = filesList(lo)
			}
			for _, itemPrefix := range lister {
				item := strings.TrimPrefix(itemPrefix, pathPrefix)
				item = strings.TrimSuffix(item, "/")
				select {
				case ch <- itemOrErr{Item: item}:
				case <-doneCh:
					close(ch)
					return
				}
			}
			if !lo.IsTruncated {
				close(ch)
				return
			}
		}
	}()
	return ch
}

func loadIAMConfigItemEtcd(ctx context.Context, item interface{}, path string) error {
	pdata, err := readKeyEtcd(ctx, globalEtcdClient, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(pdata, item)
}

func saveIAMConfigItemEtcd(ctx context.Context, item interface{}, path string) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	return saveKeyEtcd(ctx, globalEtcdClient, path, data)
}

// IAMSys - config system.
type IAMSys struct {
	sync.RWMutex
	// map of policy names to policy definitions
	iamPolicyDocsMap map[string]iampolicy.Policy
	// map of usernames to credentials
	iamUsersMap map[string]auth.Credentials
	// map of group names to group info
	iamGroupsMap map[string]GroupInfo
	// map of user names to groups they are a member of
	iamUserGroupMemberships map[string]set.StringSet
	// map of usernames/temporary access keys to policy names
	iamUserPolicyMap map[string]MappedPolicy
	// map of group names to policy names
	iamGroupPolicyMap map[string]MappedPolicy
}

func loadPolicyDoc(objectAPI ObjectLayer, policy string, m map[string]iampolicy.Policy) error {
	var p iampolicy.Policy
	err := loadIAMConfigItem(objectAPI, &p, getPolicyDocPath(policy))
	if err != nil {
		return err
	}
	m[policy] = p
	return nil
}

func loadPolicyDocs(objectAPI ObjectLayer, m map[string]iampolicy.Policy) error {
	doneCh := make(chan struct{})
	defer close(doneCh)
	for item := range listIAMConfigItems(objectAPI, iamConfigPoliciesPrefix, true, doneCh) {
		if item.Err != nil {
			return item.Err
		}

		policyName := item.Item
		err := loadPolicyDoc(objectAPI, policyName, m)
		if err != nil {
			return err
		}
	}
	return nil
}

func loadUser(objectAPI ObjectLayer, user string, isSTS bool,
	m map[string]auth.Credentials) error {

	var u UserIdentity
	err := loadIAMConfigItem(objectAPI, &u, getUserIdentityPath(user, isSTS))
	if err != nil {
		return err
	}

	if u.Credentials.IsExpired() {
		idFile := getUserIdentityPath(user, isSTS)
		// Delete expired identity - ignoring errors here.
		deleteConfig(context.Background(), objectAPI, idFile)
		deleteConfig(context.Background(), objectAPI, getMappedPolicyPath(user, isSTS, false))
		return nil
	}

	// In some cases access key may not be set, so we set it explicitly.
	u.Credentials.AccessKey = user
	m[user] = u.Credentials
	return nil
}

func loadUsers(objectAPI ObjectLayer, isSTS bool, m map[string]auth.Credentials) error {
	doneCh := make(chan struct{})
	defer close(doneCh)
	basePrefix := iamConfigUsersPrefix
	if isSTS {
		basePrefix = iamConfigSTSPrefix
	}
	for item := range listIAMConfigItems(objectAPI, basePrefix, true, doneCh) {
		if item.Err != nil {
			return item.Err
		}

		userName := item.Item
		err := loadUser(objectAPI, userName, isSTS, m)
		if err != nil {
			return err
		}
	}
	return nil
}

func loadGroup(objectAPI ObjectLayer, group string, m map[string]GroupInfo) error {
	var g GroupInfo
	err := loadIAMConfigItem(objectAPI, &g, getGroupInfoPath(group))
	if err != nil {
		return err
	}
	m[group] = g
	return nil
}

func loadGroups(objectAPI ObjectLayer, m map[string]GroupInfo) error {
	doneCh := make(chan struct{})
	defer close(doneCh)
	for item := range listIAMConfigItems(objectAPI, iamConfigGroupsPrefix, true, doneCh) {
		if item.Err != nil {
			return item.Err
		}

		group := item.Item
		err := loadGroup(objectAPI, group, m)
		if err != nil {
			return err
		}
	}
	return nil
}

func loadMappedPolicy(objectAPI ObjectLayer, name string, isSTS, isGroup bool,
	m map[string]MappedPolicy) error {

	var p MappedPolicy
	err := loadIAMConfigItem(objectAPI, &p, getMappedPolicyPath(name, isSTS, isGroup))
	if err != nil {
		return err
	}
	m[name] = p
	return nil
}

func loadMappedPolicies(objectAPI ObjectLayer, isSTS, isGroup bool, m map[string]MappedPolicy) error {
	doneCh := make(chan struct{})
	defer close(doneCh)
	basePath := iamConfigPolicyDBUsersPrefix
	if isSTS {
		basePath = iamConfigPolicyDBSTSUsersPrefix
	}
	for item := range listIAMConfigItems(objectAPI, basePath, false, doneCh) {
		if item.Err != nil {
			return item.Err
		}

		policyFile := item.Item
		userOrGroupName := strings.TrimSuffix(policyFile, ".json")
		err := loadMappedPolicy(objectAPI, userOrGroupName, isSTS, isGroup, m)
		if err != nil {
			return err
		}
	}
	return nil
}

// LoadGroup - loads a specific group from storage, and updates the
// memberships cache. If the specified group does not exist in
// storage, it is removed from in-memory maps as well - this
// simplifies the implementation for group removal. This is called
// only via IAM notifications.
func (sys *IAMSys) LoadGroup(objAPI ObjectLayer, group string) error {
	if objAPI == nil {
		return errInvalidArgument
	}

	sys.Lock()
	defer sys.Unlock()

	if globalEtcdClient != nil {
		// Watch APIs cover this case, so nothing to do.
		return nil
	}

	err := loadGroup(objAPI, group, sys.iamGroupsMap)
	if err != nil && err != errConfigNotFound {
		return err
	}

	if err == errConfigNotFound {
		// group does not exist - so remove from memory.
		sys.removeGroupFromMembershipsMap(group)
		delete(sys.iamGroupsMap, group)
		delete(sys.iamGroupPolicyMap, group)
		return nil
	}

	gi := sys.iamGroupsMap[group]

	// Updating the group memberships cache happens in two steps:
	//
	// 1. Remove the group from each user's list of memberships.
	// 2. Add the group to each member's list of memberships.
	//
	// This ensures that regardless of members being added or
	// removed, the cache stays current.
	sys.removeGroupFromMembershipsMap(group)
	sys.updateGroupMembershipsMap(group, &gi)
	return nil
}

// LoadPolicy - reloads a specific canned policy from backend disks or etcd.
func (sys *IAMSys) LoadPolicy(objAPI ObjectLayer, policyName string) error {
	if objAPI == nil {
		return errInvalidArgument
	}

	sys.Lock()
	defer sys.Unlock()

	if globalEtcdClient == nil {
		return loadPolicyDoc(objAPI, policyName, sys.iamPolicyDocsMap)
	}

	// When etcd is set, we use watch APIs so this code is not needed.
	return nil
}

// LoadUser - reloads a specific user from backend disks or etcd.
func (sys *IAMSys) LoadUser(objAPI ObjectLayer, accessKey string, isSTS bool) error {
	if objAPI == nil {
		return errInvalidArgument
	}

	sys.Lock()
	defer sys.Unlock()

	if globalEtcdClient == nil {
		err := loadUser(objAPI, accessKey, isSTS, sys.iamUsersMap)
		if err != nil {
			return err
		}
		err = loadMappedPolicy(objAPI, accessKey, isSTS, false, sys.iamUserPolicyMap)
		if err != nil {
			return err
		}
	}
	// When etcd is set, we use watch APIs so this code is not needed.
	return nil
}

// Load - loads iam subsystem
func (sys *IAMSys) Load(objAPI ObjectLayer) error {
	if globalEtcdClient != nil {
		return sys.refreshEtcd()
	}
	return sys.refresh(objAPI)
}

// sys.RLock is held by caller.
func (sys *IAMSys) reloadFromEvent(event *etcd.Event) {
	eventCreate := event.IsModify() || event.IsCreate()
	eventDelete := event.Type == etcd.EventTypeDelete
	usersPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigUsersPrefix)
	groupsPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigGroupsPrefix)
	stsPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigSTSPrefix)
	policyPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigPoliciesPrefix)
	policyDBUsersPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigPolicyDBUsersPrefix)
	policyDBSTSUsersPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigPolicyDBSTSUsersPrefix)

	ctx, cancel := context.WithTimeout(context.Background(),
		defaultContextTimeout)
	defer cancel()

	switch {
	case eventCreate:
		switch {
		case usersPrefix:
			accessKey := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigUsersPrefix))
			loadEtcdUser(ctx, accessKey, false, sys.iamUsersMap)
		case stsPrefix:
			accessKey := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigSTSPrefix))
			loadEtcdUser(ctx, accessKey, true, sys.iamUsersMap)
		case groupsPrefix:
			group := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigGroupsPrefix))
			loadEtcdGroup(ctx, group, sys.iamGroupsMap)
			gi := sys.iamGroupsMap[group]
			sys.removeGroupFromMembershipsMap(group)
			sys.updateGroupMembershipsMap(group, &gi)
		case policyPrefix:
			policyName := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPoliciesPrefix))
			loadEtcdPolicy(ctx, policyName, sys.iamPolicyDocsMap)
		case policyDBUsersPrefix:
			policyMapFile := strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPolicyDBUsersPrefix)
			user := strings.TrimSuffix(policyMapFile, ".json")
			loadEtcdMappedPolicy(ctx, user, false, false, sys.iamUserPolicyMap)
		case policyDBSTSUsersPrefix:
			policyMapFile := strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPolicyDBSTSUsersPrefix)
			user := strings.TrimSuffix(policyMapFile, ".json")
			loadEtcdMappedPolicy(ctx, user, true, false, sys.iamUserPolicyMap)
		}
	case eventDelete:
		switch {
		case usersPrefix:
			accessKey := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigUsersPrefix))
			delete(sys.iamUsersMap, accessKey)
		case stsPrefix:
			accessKey := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigSTSPrefix))
			delete(sys.iamUsersMap, accessKey)
		case groupsPrefix:
			group := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigGroupsPrefix))
			sys.removeGroupFromMembershipsMap(group)
			delete(sys.iamGroupsMap, group)
			delete(sys.iamGroupPolicyMap, group)
		case policyPrefix:
			policyName := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPoliciesPrefix))
			delete(sys.iamPolicyDocsMap, policyName)
		case policyDBUsersPrefix:
			policyMapFile := strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPolicyDBUsersPrefix)
			user := strings.TrimSuffix(policyMapFile, ".json")
			delete(sys.iamUserPolicyMap, user)
		case policyDBSTSUsersPrefix:
			policyMapFile := strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPolicyDBSTSUsersPrefix)
			user := strings.TrimSuffix(policyMapFile, ".json")
			delete(sys.iamUserPolicyMap, user)
		}
	}
}

// Watch etcd entries for IAM
func (sys *IAMSys) watchIAMEtcd() {
	watchEtcd := func() {
		// Refresh IAMSys with etcd watch.
	mainLoop:
		for {
			watchCh := globalEtcdClient.Watch(context.Background(),
				iamConfigPrefix, etcd.WithPrefix(), etcd.WithKeysOnly())
			for {
				select {
				case <-GlobalServiceDoneCh:
					return
				case watchResp, ok := <-watchCh:
					if !ok {
						goto mainLoop
					}
					if err := watchResp.Err(); err != nil {
						logger.LogIf(context.Background(), err)
						// log and retry.
						continue
					}
					sys.Lock()
					for _, event := range watchResp.Events {
						sys.reloadFromEvent(event)
					}
					sys.Unlock()
				}
			}
		}
	}
	go watchEtcd()
}

func (sys *IAMSys) watchIAMDisk(objAPI ObjectLayer) {
	watchDisk := func() {
		ticker := time.NewTicker(globalRefreshIAMInterval)
		defer ticker.Stop()
		for {
			select {
			case <-GlobalServiceDoneCh:
				return
			case <-ticker.C:
				sys.refresh(objAPI)
			}
		}
	}
	// Refresh IAMSys in background.
	go watchDisk()
}

// Migrate users directory in a single scan.
//
// 1. Migrate user policy from:
//
// `iamConfigUsersPrefix + "<username>/policy.json"`
//
// to:
//
// `iamConfigPolicyDBUsersPrefix + "<username>.json"`.
//
// 2. Add versioning to the policy json file in the new
// location.
//
// 3. Migrate user identity json file to include version info.
func migrateUsersConfigToV1(objAPI ObjectLayer, isSTS bool) error {
	basePrefix := iamConfigUsersPrefix
	if isSTS {
		basePrefix = iamConfigSTSPrefix
	}

	doneCh := make(chan struct{})
	defer close(doneCh)
	for item := range listIAMConfigItems(objAPI, basePrefix, true, doneCh) {
		if item.Err != nil {
			return item.Err
		}

		user := item.Item

		{
			// 1. check if there is policy file in old location.
			oldPolicyPath := pathJoin(basePrefix, user, iamPolicyFile)
			var policyName string
			if err := loadIAMConfigItem(objAPI, &policyName, oldPolicyPath); err != nil {
				switch err {
				case errConfigNotFound:
					// This case means it is already
					// migrated or there is no policy on
					// user.
				default:
					// File may be corrupt or network error
				}

				// Nothing to do on the policy file,
				// so move on to check the id file.
				goto next
			}

			// 2. copy policy file to new location.
			mp := newMappedPolicy(policyName)
			path := getMappedPolicyPath(user, isSTS, false)
			if err := saveIAMConfigItem(objAPI, mp, path); err != nil {
				return err
			}

			// 3. delete policy file from old
			// location. Ignore error.
			deleteConfig(context.Background(), objAPI, oldPolicyPath)
		}
	next:
		// 4. check if user identity has old format.
		identityPath := pathJoin(basePrefix, user, iamIdentityFile)
		var cred auth.Credentials
		if err := loadIAMConfigItem(objAPI, &cred, identityPath); err != nil {
			switch err.(type) {
			case ObjectNotFound:
				// This should not happen.
			default:
				// File may be corrupt or network error
			}
			continue
		}

		// If the file is already in the new format,
		// then the parsed auth.Credentials will have
		// the zero value for the struct.
		var zeroCred auth.Credentials
		if cred == zeroCred {
			// nothing to do
			continue
		}

		// Found a id file in old format. Copy value
		// into new format and save it.
		cred.AccessKey = user
		u := newUserIdentity(cred)
		if err := saveIAMConfigItem(objAPI, u, identityPath); err != nil {
			logger.LogIf(context.Background(), err)
			return err
		}

		// Nothing to delete as identity file location
		// has not changed.
	}
	return nil
}

// migrateUsersConfigEtcd - same as migrateUsersConfig but on etcd.
func migrateUsersConfigEtcdToV1(isSTS bool) error {
	basePrefix := iamConfigUsersPrefix
	if isSTS {
		basePrefix = iamConfigSTSPrefix
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultContextTimeout)
	defer cancel()
	r, err := globalEtcdClient.Get(ctx, basePrefix, etcd.WithPrefix(), etcd.WithKeysOnly())
	if err != nil {
		return err
	}

	users := etcdKvsToSet(basePrefix, r.Kvs)
	for _, user := range users.ToSlice() {
		{
			// 1. check if there is a policy file in the old loc.
			oldPolicyPath := pathJoin(basePrefix, user, iamPolicyFile)
			var policyName string
			err := loadIAMConfigItemEtcd(ctx, &policyName, oldPolicyPath)
			if err != nil {
				switch err {
				case errConfigNotFound:
					// No mapped policy or already migrated.
				default:
					// corrupt data/read error, etc
				}
				goto next
			}

			// 2. copy policy to new loc.
			mp := newMappedPolicy(policyName)
			path := getMappedPolicyPath(user, isSTS, false)
			if err := saveIAMConfigItemEtcd(ctx, mp, path); err != nil {
				return err
			}

			// 3. delete policy file in old loc.
			deleteKeyEtcd(ctx, globalEtcdClient, oldPolicyPath)
		}

	next:
		// 4. check if user identity has old format.
		identityPath := pathJoin(basePrefix, user, iamIdentityFile)
		var cred auth.Credentials
		if err := loadIAMConfigItemEtcd(ctx, &cred, identityPath); err != nil {
			switch err {
			case errConfigNotFound:
				// This case should not happen.
			default:
				// corrupt file or read error
			}
			continue
		}

		// If the file is already in the new format,
		// then the parsed auth.Credentials will have
		// the zero value for the struct.
		var zeroCred auth.Credentials
		if cred == zeroCred {
			// nothing to do
			continue
		}

		// Found a id file in old format. Copy value
		// into new format and save it.
		u := newUserIdentity(cred)
		if err := saveIAMConfigItemEtcd(ctx, u, identityPath); err != nil {
			logger.LogIf(context.Background(), err)
			return err
		}

		// Nothing to delete as identity file location
		// has not changed.
	}
	return nil

}

func migrateToV1(objAPI ObjectLayer) error {
	currentIAMFormat := getCurrentIAMFormat()
	path := getIAMFormatFilePath()

	if globalEtcdClient == nil {
		var iamFmt iamFormat
		if err := loadIAMConfigItem(objAPI, &iamFmt, path); err != nil {
			switch err {
			case errConfigNotFound:
				// Need to migrate to V1.
			default:
				return errors.New("corrupt IAM format file")
			}
		} else {
			if iamFmt.Version >= currentIAMFormat.Version {
				// Already migrated to V1 or higher!
				return nil
			}
			// This case should not happen
			// (i.e. Version is 0 or negative.)
			return errors.New("got an invalid IAM format version")
		}

		// Migrate long-term users
		if err := migrateUsersConfigToV1(objAPI, false); err != nil {
			logger.LogIf(context.Background(), err)
			return err
		}
		// Migrate STS users
		if err := migrateUsersConfigToV1(objAPI, true); err != nil {
			logger.LogIf(context.Background(), err)
			return err
		}
		// Save iam version file.
		if err := saveIAMConfigItem(objAPI, currentIAMFormat, path); err != nil {
			logger.LogIf(context.Background(), err)
			return err
		}
	} else {
		var iamFmt iamFormat
		if err := loadIAMConfigItemEtcd(context.Background(), &iamFmt, path); err != nil {
			switch err {
			case errConfigNotFound:
				// Need to migrate to V1.
			default:
				return errors.New("corrupt IAM format file")
			}
		} else {
			if iamFmt.Version >= currentIAMFormat.Version {
				// Already migrated to V1 of higher!
				return nil
			}
			// This case should not happen
			// (i.e. Version is 0 or negative.)
			return errors.New("got an invalid IAM format version")

		}

		// Migrate long-term users
		if err := migrateUsersConfigEtcdToV1(false); err != nil {
			logger.LogIf(context.Background(), err)
			return err
		}
		// Migrate STS users
		if err := migrateUsersConfigEtcdToV1(true); err != nil {
			logger.LogIf(context.Background(), err)
			return err
		}
		// Save iam version file.
		if err := saveIAMConfigItemEtcd(context.Background(), currentIAMFormat, path); err != nil {
			logger.LogIf(context.Background(), err)
			return err
		}
	}
	return nil
}

// Perform IAM configuration migration.
func doIAMConfigMigration(objAPI ObjectLayer) error {
	// Take IAM configuration migration lock
	lockPath := iamConfigPrefix + "/migration.lock"
	objLock := globalNSMutex.NewNSLock(context.Background(), minioMetaBucket, lockPath)
	if err := objLock.GetLock(globalOperationTimeout); err != nil {
		return err
	}
	defer objLock.Unlock()

	if err := migrateToV1(objAPI); err != nil {
		return err
	}
	// Add future IAM migrations here.
	return nil
}

// Init - initializes config system from iam.json
func (sys *IAMSys) Init(objAPI ObjectLayer) error {
	if objAPI == nil {
		return errInvalidArgument
	}

	doneCh := make(chan struct{})
	defer close(doneCh)

	// Migrating IAM needs a retry mechanism for
	// the following reasons:
	//  - Read quorum is lost just after the initialization
	//    of the object layer.
	for range newRetryTimerSimple(doneCh) {
		// Migrate IAM configuration
		if err := doIAMConfigMigration(objAPI); err != nil {
			if err == errDiskNotFound ||
				strings.Contains(err.Error(), InsufficientReadQuorum{}.Error()) ||
				strings.Contains(err.Error(), InsufficientWriteQuorum{}.Error()) {
				logger.Info("Waiting for IAM subsystem to be initialized..")
				continue
			}
			return err
		}
		break
	}

	if globalEtcdClient != nil {
		defer sys.watchIAMEtcd()
	} else {
		defer sys.watchIAMDisk(objAPI)
	}

	// Initializing IAM needs a retry mechanism for
	// the following reasons:
	//  - Read quorum is lost just after the initialization
	//    of the object layer.
	for range newRetryTimerSimple(doneCh) {
		if globalEtcdClient != nil {
			return sys.refreshEtcd()
		}
		// Load IAMSys once during boot.
		if err := sys.refresh(objAPI); err != nil {
			if err == errDiskNotFound ||
				strings.Contains(err.Error(), InsufficientReadQuorum{}.Error()) ||
				strings.Contains(err.Error(), InsufficientWriteQuorum{}.Error()) {
				logger.Info("Waiting for IAM subsystem to be initialized..")
				continue
			}
			return err
		}
		break
	}

	return nil
}

// DeletePolicy - deletes a canned policy from backend or etcd.
func (sys *IAMSys) DeletePolicy(policyName string) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	if policyName == "" {
		return errInvalidArgument
	}

	var err error
	pFile := getPolicyDocPath(policyName)
	if globalEtcdClient != nil {
		err = deleteKeyEtcd(context.Background(), globalEtcdClient, pFile)
	} else {
		err = deleteConfig(context.Background(), objectAPI, pFile)
	}
	switch err.(type) {
	case ObjectNotFound:
		// Ignore error if policy is already deleted.
		err = nil
	}

	sys.Lock()
	defer sys.Unlock()

	delete(sys.iamPolicyDocsMap, policyName)
	return err
}

// ListPolicies - lists all canned policies.
func (sys *IAMSys) ListPolicies() (map[string][]byte, error) {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return nil, errServerNotInitialized
	}

	var policyDocsMap = make(map[string][]byte)

	sys.RLock()
	defer sys.RUnlock()

	for k, v := range sys.iamPolicyDocsMap {
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		policyDocsMap[k] = data
	}

	return policyDocsMap, nil
}

// SetPolicy - sets a new name policy.
func (sys *IAMSys) SetPolicy(policyName string, p iampolicy.Policy) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	if p.IsEmpty() || policyName == "" {
		return errInvalidArgument
	}

	path := getPolicyDocPath(policyName)
	var err error
	if globalEtcdClient != nil {
		err = saveIAMConfigItemEtcd(context.Background(), p, path)
	} else {
		err = saveIAMConfigItem(objectAPI, p, path)
	}
	if err != nil {
		return err
	}

	sys.Lock()
	defer sys.Unlock()

	sys.iamPolicyDocsMap[policyName] = p

	return nil
}

// DeleteUser - delete user (only for long-term users not STS users).
func (sys *IAMSys) DeleteUser(accessKey string) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	var err error
	mappingPath := getMappedPolicyPath(accessKey, false, false)
	idPath := getUserIdentityPath(accessKey, false)
	if globalEtcdClient != nil {
		// It is okay to ignore errors when deleting policy.json for the user.
		deleteKeyEtcd(context.Background(), globalEtcdClient, mappingPath)
		err = deleteKeyEtcd(context.Background(), globalEtcdClient, idPath)
	} else {
		// It is okay to ignore errors when deleting policy.json for the user.
		_ = deleteConfig(context.Background(), objectAPI, mappingPath)
		err = deleteConfig(context.Background(), objectAPI, idPath)
	}

	switch err.(type) {
	case ObjectNotFound:
		// ignore if user is already deleted.
		err = nil
	}

	sys.Lock()
	defer sys.Unlock()

	delete(sys.iamUsersMap, accessKey)
	delete(sys.iamUserPolicyMap, accessKey)

	return err
}

// SetTempUser - set temporary user credentials, these credentials have an expiry.
func (sys *IAMSys) SetTempUser(accessKey string, cred auth.Credentials, policyName string) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	sys.Lock()
	defer sys.Unlock()

	// If OPA is not set we honor any policy claims for this
	// temporary user which match with pre-configured canned
	// policies for this server.
	if globalPolicyOPA == nil && policyName != "" {
		p, ok := sys.iamPolicyDocsMap[policyName]
		if !ok {
			return errInvalidArgument
		}
		if p.IsEmpty() {
			delete(sys.iamUserPolicyMap, accessKey)
			return nil
		}

		mp := newMappedPolicy(policyName)

		mappingPath := getMappedPolicyPath(accessKey, true, false)
		var err error
		if globalEtcdClient != nil {
			err = saveIAMConfigItemEtcd(context.Background(), mp, mappingPath)
		} else {
			err = saveIAMConfigItem(objectAPI, mp, mappingPath)
		}
		if err != nil {
			return err
		}

		sys.iamUserPolicyMap[accessKey] = mp
	}

	idPath := getUserIdentityPath(accessKey, true)
	u := newUserIdentity(cred)

	var err error
	if globalEtcdClient != nil {
		err = saveIAMConfigItemEtcd(context.Background(), u, idPath)
	} else {
		err = saveIAMConfigItem(objectAPI, u, idPath)
	}
	if err != nil {
		return err
	}

	sys.iamUsersMap[accessKey] = cred
	return nil
}

// ListUsers - list all users.
func (sys *IAMSys) ListUsers() (map[string]madmin.UserInfo, error) {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return nil, errServerNotInitialized
	}

	var users = make(map[string]madmin.UserInfo)

	sys.RLock()
	defer sys.RUnlock()

	for k, v := range sys.iamUsersMap {
		users[k] = madmin.UserInfo{
			PolicyName: sys.iamUserPolicyMap[k].Policy,
			Status:     madmin.AccountStatus(v.Status),
		}
	}

	return users, nil
}

// SetUserStatus - sets current user status, supports disabled or enabled.
func (sys *IAMSys) SetUserStatus(accessKey string, status madmin.AccountStatus) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	if status != madmin.AccountEnabled && status != madmin.AccountDisabled {
		return errInvalidArgument
	}

	sys.Lock()
	defer sys.Unlock()

	cred, ok := sys.iamUsersMap[accessKey]
	if !ok {
		return errNoSuchUser
	}

	uinfo := newUserIdentity(auth.Credentials{
		AccessKey: accessKey,
		SecretKey: cred.SecretKey,
		Status:    string(status),
	})
	idFile := getUserIdentityPath(accessKey, false)
	var err error
	if globalEtcdClient != nil {
		err = saveIAMConfigItemEtcd(context.Background(), uinfo, idFile)
	} else {
		err = saveIAMConfigItem(objectAPI, uinfo, idFile)
	}
	if err != nil {
		return err
	}

	sys.iamUsersMap[accessKey] = uinfo.Credentials
	return nil
}

// SetUser - set user credentials and policy.
func (sys *IAMSys) SetUser(accessKey string, uinfo madmin.UserInfo) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	idFile := getUserIdentityPath(accessKey, false)
	u := newUserIdentity(auth.Credentials{
		AccessKey: accessKey,
		SecretKey: uinfo.SecretKey,
		Status:    string(uinfo.Status),
	})

	sys.Lock()
	defer sys.Unlock()

	var err error
	if globalEtcdClient != nil {
		err = saveIAMConfigItemEtcd(context.Background(), u, idFile)
	} else {
		err = saveIAMConfigItem(objectAPI, u, idFile)
	}
	if err != nil {
		return err
	}
	sys.iamUsersMap[accessKey] = u.Credentials

	// Set policy if specified.
	if uinfo.PolicyName != "" {
		return sys.policyDBSet(objectAPI, accessKey, uinfo.PolicyName, false, false)
	}
	return nil
}

// SetUserSecretKey - sets user secret key
func (sys *IAMSys) SetUserSecretKey(accessKey string, secretKey string) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	sys.Lock()
	defer sys.Unlock()

	cred, ok := sys.iamUsersMap[accessKey]
	if !ok {
		return errNoSuchUser
	}

	cred.SecretKey = secretKey
	u := newUserIdentity(cred)
	idFile := getUserIdentityPath(accessKey, false)

	var err error
	if globalEtcdClient != nil {
		err = saveIAMConfigItemEtcd(context.Background(), u, idFile)
	} else {
		err = saveIAMConfigItem(objectAPI, u, idFile)
	}
	if err != nil {
		return err
	}

	sys.iamUsersMap[accessKey] = cred
	return nil
}

// GetUser - get user credentials
func (sys *IAMSys) GetUser(accessKey string) (cred auth.Credentials, ok bool) {
	sys.RLock()
	defer sys.RUnlock()

	cred, ok = sys.iamUsersMap[accessKey]
	return cred, ok && cred.IsValid()
}

// AddUsersToGroup - adds users to a group, creating the group if
// needed. No error if user(s) already are in the group.
func (sys *IAMSys) AddUsersToGroup(group string, members []string) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	if group == "" {
		return errInvalidArgument
	}

	sys.Lock()
	defer sys.Unlock()

	// Validate that all members exist.
	for _, member := range members {
		_, ok := sys.iamUsersMap[member]
		if !ok {
			return errNoSuchUser
		}
	}

	gi, ok := sys.iamGroupsMap[group]
	if !ok {
		// Set group as enabled by default when it doesn't
		// exist.
		gi = newGroupInfo(members)
	} else {
		mergedMembers := append(gi.Members, members...)
		uniqMembers := set.CreateStringSet(mergedMembers...).ToSlice()
		gi.Members = uniqMembers
	}

	var err error
	if globalEtcdClient == nil {
		err = saveIAMConfigItem(objectAPI, gi, getGroupInfoPath(group))
	} else {
		err = saveIAMConfigItemEtcd(context.Background(), gi, getGroupInfoPath(group))
	}
	if err != nil {
		return err
	}

	sys.iamGroupsMap[group] = gi

	// update user-group membership map
	for _, member := range members {
		gset := sys.iamUserGroupMemberships[member]
		if gset == nil {
			gset = set.CreateStringSet(group)
		} else {
			gset.Add(group)
		}
		sys.iamUserGroupMemberships[member] = gset
	}

	return nil
}

// RemoveUsersFromGroup - remove users from group. If no users are
// given, and the group is empty, deletes the group as well.
func (sys *IAMSys) RemoveUsersFromGroup(group string, members []string) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	if group == "" {
		return errInvalidArgument
	}

	sys.Lock()
	defer sys.Unlock()

	// Validate that all members exist.
	for _, member := range members {
		_, ok := sys.iamUsersMap[member]
		if !ok {
			return errNoSuchUser
		}
	}

	gi, ok := sys.iamGroupsMap[group]
	if !ok {
		return errNoSuchGroup
	}

	// Check if attempting to delete a non-empty group.
	if len(members) == 0 && len(gi.Members) != 0 {
		return errGroupNotEmpty
	}

	if len(members) == 0 {
		// len(gi.Members) == 0 here.

		// Remove the group from storage.
		giPath := getGroupInfoPath(group)
		giPolicyPath := getMappedPolicyPath(group, false, true)
		if globalEtcdClient == nil {
			err := deleteConfig(context.Background(), objectAPI, giPath)
			if err != nil {
				return err
			}

			// Remove mapped policy from storage.
			err = deleteConfig(context.Background(), objectAPI,
				getMappedPolicyPath(group, false, true))
			if err != nil {
				return err
			}
		} else {
			err := deleteKeyEtcd(context.Background(), globalEtcdClient, giPath)
			if err != nil {
				return err
			}

			err = deleteKeyEtcd(context.Background(), globalEtcdClient, giPolicyPath)
			if err != nil {
				return err
			}
		}

		// Delete from server memory
		delete(sys.iamGroupsMap, group)
		delete(sys.iamGroupPolicyMap, group)
		return nil
	}

	// Only removing members.
	s := set.CreateStringSet(gi.Members...)
	d := set.CreateStringSet(members...)
	gi.Members = s.Difference(d).ToSlice()

	err := saveIAMConfigItem(objectAPI, gi, getGroupInfoPath(group))
	if err != nil {
		return err
	}
	sys.iamGroupsMap[group] = gi

	// update user-group membership map
	for _, member := range members {
		gset := sys.iamUserGroupMemberships[member]
		if gset == nil {
			continue
		}
		gset.Remove(group)
		sys.iamUserGroupMemberships[member] = gset
	}

	return nil
}

// SetGroupStatus - enable/disabled a group
func (sys *IAMSys) SetGroupStatus(group string, enabled bool) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	sys.Lock()
	defer sys.Unlock()

	if group == "" {
		return errInvalidArgument
	}

	gi, ok := sys.iamGroupsMap[group]
	if !ok {
		return errNoSuchGroup
	}

	if enabled {
		gi.Status = statusEnabled
	} else {
		gi.Status = statusDisabled
	}

	var err error
	if globalEtcdClient == nil {
		err = saveIAMConfigItem(objectAPI, gi, getGroupInfoPath(group))
	} else {
		err = saveIAMConfigItemEtcd(context.Background(), gi, getGroupInfoPath(group))
	}
	if err != nil {
		return err
	}

	sys.iamGroupsMap[group] = gi

	return nil
}

// GetGroupDescription - builds up group description
func (sys *IAMSys) GetGroupDescription(group string) (gd madmin.GroupDesc, err error) {
	sys.RLock()
	defer sys.RUnlock()

	gi, ok := sys.iamGroupsMap[group]
	if !ok {
		return gd, errNoSuchGroup
	}

	var p []string
	p, err = sys.policyDBGet(group, true)
	if err != nil {
		return gd, err
	}

	policy := ""
	if len(p) > 0 {
		policy = p[0]
	}

	return madmin.GroupDesc{
		Name:    group,
		Status:  gi.Status,
		Members: gi.Members,
		Policy:  policy,
	}, nil
}

// ListGroups - lists groups
func (sys *IAMSys) ListGroups() (r []string) {
	sys.RLock()
	defer sys.RUnlock()

	for k := range sys.iamGroupsMap {
		r = append(r, k)
	}
	return r
}

// PolicyDBSet - sets a policy for a user or group in the
// PolicyDB. This function applies only long-term users. For STS
// users, policy is set directly by called sys.policyDBSet().
func (sys *IAMSys) PolicyDBSet(name, policy string, isGroup bool) error {
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return errServerNotInitialized
	}

	sys.Lock()
	defer sys.Unlock()

	// isSTS is always false when called via PolicyDBSet as policy
	// is never set by an external API call for STS users.
	return sys.policyDBSet(objectAPI, name, policy, false, isGroup)
}

// policyDBSet - sets a policy for user in the policy db. Assumes that
// caller has sys.Lock().
func (sys *IAMSys) policyDBSet(objectAPI ObjectLayer, name, policy string, isSTS, isGroup bool) error {
	if name == "" || policy == "" {
		return errInvalidArgument
	}

	if _, ok := sys.iamUsersMap[name]; !ok {
		return errNoSuchUser
	}

	_, ok := sys.iamPolicyDocsMap[policy]
	if !ok {
		return errNoSuchPolicy
	}

	mp := newMappedPolicy(policy)
	_, ok = sys.iamUsersMap[name]
	if !ok {
		return errNoSuchUser
	}
	var err error
	mappingPath := getMappedPolicyPath(name, isSTS, isGroup)
	if globalEtcdClient != nil {
		err = saveIAMConfigItemEtcd(context.Background(), mp, mappingPath)
	} else {
		err = saveIAMConfigItem(objectAPI, mp, mappingPath)
	}
	if err != nil {
		return err
	}
	sys.iamUserPolicyMap[name] = mp
	return nil
}

// PolicyDBGet - gets policy set on a user or group. Since a user may
// be a member of multiple groups, this function returns an array of
// applicable policies (each group is mapped to at most one policy).
func (sys *IAMSys) PolicyDBGet(name string, isGroup bool) ([]string, error) {
	if name == "" {
		return nil, errInvalidArgument
	}

	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		return nil, errServerNotInitialized
	}

	sys.RLock()
	defer sys.RUnlock()

	return sys.policyDBGet(name, isGroup)
}

// This call assumes that caller has the sys.RLock()
func (sys *IAMSys) policyDBGet(name string, isGroup bool) ([]string, error) {
	if isGroup {
		if _, ok := sys.iamGroupsMap[name]; !ok {
			return nil, errNoSuchGroup
		}

		policy := sys.iamGroupPolicyMap[name]
		// returned policy could be empty
		if policy.Policy == "" {
			return nil, nil
		}
		return []string{policy.Policy}, nil
	}

	if _, ok := sys.iamUsersMap[name]; !ok {
		return nil, errNoSuchUser
	}

	result := []string{}
	policy := sys.iamUserPolicyMap[name]
	// returned policy could be empty
	if policy.Policy != "" {
		result = append(result, policy.Policy)
	}
	for _, group := range sys.iamUserGroupMemberships[name].ToSlice() {
		p, ok := sys.iamGroupPolicyMap[group]
		if ok && p.Policy != "" {
			result = append(result, p.Policy)
		}
	}
	return result, nil
}

// IsAllowedSTS is meant for STS based temporary credentials,
// which implements claims validation and verification other than
// applying policies.
func (sys *IAMSys) IsAllowedSTS(args iampolicy.Args) bool {
	pname, ok := args.Claims[iampolicy.PolicyName]
	if !ok {
		// When claims are set, it should have a "policy" field.
		return false
	}
	pnameStr, ok := pname.(string)
	if !ok {
		// When claims has "policy" field, it should be string.
		return false
	}

	sys.RLock()
	defer sys.RUnlock()

	// If policy is available for given user, check the policy.
	mp, ok := sys.iamUserPolicyMap[args.AccountName]
	if !ok {
		// No policy available reject.
		return false
	}
	name := mp.Policy

	if pnameStr != name {
		// When claims has a policy, it should match the
		// policy of args.AccountName which server remembers.
		// if not reject such requests.
		return false
	}

	// Now check if we have a sessionPolicy.
	spolicy, ok := args.Claims[iampolicy.SessionPolicyName]
	if !ok {
		// Sub policy not set, this is most common since subPolicy
		// is optional, use the top level policy only.
		p, ok := sys.iamPolicyDocsMap[pnameStr]
		return ok && p.IsAllowed(args)
	}

	spolicyStr, ok := spolicy.(string)
	if !ok {
		// Sub policy if set, should be a string reject
		// malformed/malicious requests.
		return false
	}

	// Check if policy is parseable.
	subPolicy, err := iampolicy.ParseConfig(bytes.NewReader([]byte(spolicyStr)))
	if err != nil {
		// Log any error in input session policy config.
		logger.LogIf(context.Background(), err)
		return false
	}

	// Policy without Version string value reject it.
	if subPolicy.Version == "" {
		return false
	}

	// Sub policy is set and valid.
	p, ok := sys.iamPolicyDocsMap[pnameStr]
	return ok && p.IsAllowed(args) && subPolicy.IsAllowed(args)
}

// IsAllowed - checks given policy args is allowed to continue the Rest API.
func (sys *IAMSys) IsAllowed(args iampolicy.Args) bool {
	// If opa is configured, use OPA always.
	if globalPolicyOPA != nil {
		ok, err := globalPolicyOPA.IsAllowed(args)
		if err != nil {
			logger.LogIf(context.Background(), err)
		}
		return ok
	}

	// With claims set, we should do STS related checks and validation.
	if len(args.Claims) > 0 {
		return sys.IsAllowedSTS(args)
	}

	sys.RLock()
	defer sys.RUnlock()

	// If policy is available for given user, check the policy.
	if mp, found := sys.iamUserPolicyMap[args.AccountName]; found {
		p, ok := sys.iamPolicyDocsMap[mp.Policy]
		return ok && p.IsAllowed(args)
	}

	// As policy is not available and OPA is not configured,
	// return the owner value.
	return args.IsOwner
}

var defaultContextTimeout = 30 * time.Second

func etcdKvsToSet(prefix string, kvs []*mvccpb.KeyValue) set.StringSet {
	users := set.NewStringSet()
	for _, kv := range kvs {
		// Extract user by stripping off the `prefix` value as suffix,
		// then strip off the remaining basename to obtain the prefix
		// value, usually in the following form.
		//
		//  key := "config/iam/users/newuser/identity.json"
		//  prefix := "config/iam/users/"
		//  v := trim(trim(key, prefix), base(key)) == "newuser"
		//
		user := path.Clean(strings.TrimSuffix(strings.TrimPrefix(string(kv.Key), prefix), path.Base(string(kv.Key))))
		if !users.Contains(user) {
			users.Add(user)
		}
	}
	return users
}

func etcdKvsToSetPolicyDB(prefix string, kvs []*mvccpb.KeyValue) set.StringSet {
	items := set.NewStringSet()
	for _, kv := range kvs {
		// Extract user item by stripping off prefix and then
		// stripping of ".json" suffix.
		//
		// key := "config/iam/policydb/users/myuser1.json"
		// prefix := "config/iam/policydb/users/"
		// v := trimSuffix(trimPrefix(key, prefix), ".json")
		key := string(kv.Key)
		item := path.Clean(strings.TrimSuffix(strings.TrimPrefix(key, prefix), ".json"))
		items.Add(item)
	}
	return items
}

func loadEtcdMappedPolicy(ctx context.Context, name string, isSTS, isGroup bool,
	m map[string]MappedPolicy) error {

	var p MappedPolicy
	err := loadIAMConfigItemEtcd(ctx, &p, getMappedPolicyPath(name, isSTS, isGroup))
	if err != nil {
		return err
	}
	m[name] = p
	return nil
}

func loadEtcdMappedPolicies(isSTS, isGroup bool, m map[string]MappedPolicy) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultContextTimeout)
	defer cancel()
	basePrefix := iamConfigPolicyDBUsersPrefix
	if isSTS {
		basePrefix = iamConfigPolicyDBSTSUsersPrefix
	}
	r, err := globalEtcdClient.Get(ctx, basePrefix, etcd.WithPrefix(), etcd.WithKeysOnly())
	if err != nil {
		return err
	}

	users := etcdKvsToSetPolicyDB(basePrefix, r.Kvs)

	// Reload config and policies for all users.
	for _, user := range users.ToSlice() {
		if err = loadEtcdMappedPolicy(ctx, user, isSTS, isGroup, m); err != nil {
			return err
		}
	}
	return nil
}

func loadEtcdUser(ctx context.Context, user string, isSTS bool, m map[string]auth.Credentials) error {
	var u UserIdentity
	err := loadIAMConfigItemEtcd(ctx, &u, getUserIdentityPath(user, isSTS))
	if err != nil {
		return err
	}

	if u.Credentials.IsExpired() {
		// Delete expired identity.
		deleteKeyEtcd(ctx, globalEtcdClient, getUserIdentityPath(user, isSTS))
		deleteKeyEtcd(ctx, globalEtcdClient, getMappedPolicyPath(user, isSTS, false))
		return nil
	}

	m[user] = u.Credentials
	return nil
}

func loadEtcdUsers(isSTS bool, m map[string]auth.Credentials) error {
	basePrefix := iamConfigUsersPrefix
	if isSTS {
		basePrefix = iamConfigSTSPrefix
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultContextTimeout)
	defer cancel()
	r, err := globalEtcdClient.Get(ctx, basePrefix, etcd.WithPrefix(), etcd.WithKeysOnly())
	if err != nil {
		return err
	}

	users := etcdKvsToSet(basePrefix, r.Kvs)

	// Reload config for all users.
	for _, user := range users.ToSlice() {
		if err = loadEtcdUser(ctx, user, isSTS, m); err != nil {
			return err
		}
	}
	return nil
}

func loadEtcdGroup(ctx context.Context, group string, m map[string]GroupInfo) error {
	var gi GroupInfo
	err := loadIAMConfigItemEtcd(ctx, &gi, getGroupInfoPath(group))
	if err != nil {
		return err
	}
	m[group] = gi
	return nil
}

func loadEtcdGroups(m map[string]GroupInfo) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultContextTimeout)
	defer cancel()
	r, err := globalEtcdClient.Get(ctx, iamConfigGroupsPrefix, etcd.WithPrefix(), etcd.WithKeysOnly())
	if err != nil {
		return err
	}

	groups := etcdKvsToSet(iamConfigGroupsPrefix, r.Kvs)

	// Reload config for all groups.
	for _, group := range groups.ToSlice() {
		if err = loadEtcdGroup(ctx, group, m); err != nil {
			return err
		}
	}
	return nil
}

func loadEtcdPolicy(ctx context.Context, policyName string, m map[string]iampolicy.Policy) error {
	var p iampolicy.Policy
	err := loadIAMConfigItemEtcd(ctx, &p, getPolicyDocPath(policyName))
	if err != nil {
		return err
	}
	m[policyName] = p
	return nil
}

func loadEtcdPolicies(policyDocsMap map[string]iampolicy.Policy) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultContextTimeout)
	defer cancel()
	r, err := globalEtcdClient.Get(ctx, iamConfigPoliciesPrefix, etcd.WithPrefix(), etcd.WithKeysOnly())
	if err != nil {
		return err
	}

	policies := etcdKvsToSet(iamConfigPoliciesPrefix, r.Kvs)

	// Reload config and policies for all policys.
	for _, policyName := range policies.ToSlice() {
		err = loadEtcdPolicy(ctx, policyName, policyDocsMap)
		if err != nil {
			return err
		}
	}
	return nil
}

// Set default canned policies only if not already overridden by users.
func setDefaultCannedPolicies(policies map[string]iampolicy.Policy) {
	_, ok := policies["writeonly"]
	if !ok {
		policies["writeonly"] = iampolicy.WriteOnly
	}
	_, ok = policies["readonly"]
	if !ok {
		policies["readonly"] = iampolicy.ReadOnly
	}
	_, ok = policies["readwrite"]
	if !ok {
		policies["readwrite"] = iampolicy.ReadWrite
	}
}

func (sys *IAMSys) refreshEtcd() error {
	iamUsersMap := make(map[string]auth.Credentials)
	iamGroupsMap := make(map[string]GroupInfo)
	iamPolicyDocsMap := make(map[string]iampolicy.Policy)
	iamUserPolicyMap := make(map[string]MappedPolicy)
	iamGroupPolicyMap := make(map[string]MappedPolicy)

	if err := loadEtcdPolicies(iamPolicyDocsMap); err != nil {
		return err
	}
	if err := loadEtcdUsers(false, iamUsersMap); err != nil {
		return err
	}
	// load STS temp users into the same map
	if err := loadEtcdUsers(true, iamUsersMap); err != nil {
		return err
	}
	if err := loadEtcdGroups(iamGroupsMap); err != nil {
		return err
	}

	if err := loadEtcdMappedPolicies(false, false, iamUserPolicyMap); err != nil {
		return err
	}
	// load STS users policy mapping into the same map
	if err := loadEtcdMappedPolicies(true, false, iamUserPolicyMap); err != nil {
		return err
	}
	// load group policy mapping
	if err := loadEtcdMappedPolicies(false, true, iamGroupPolicyMap); err != nil {
		return err
	}

	// Sets default canned policies, if none are set.
	setDefaultCannedPolicies(iamPolicyDocsMap)

	sys.Lock()
	defer sys.Unlock()

	sys.iamUsersMap = iamUsersMap
	sys.iamGroupsMap = iamGroupsMap
	sys.iamUserPolicyMap = iamUserPolicyMap
	sys.iamPolicyDocsMap = iamPolicyDocsMap
	sys.iamGroupPolicyMap = iamGroupPolicyMap

	return nil
}

// Refresh IAMSys.
func (sys *IAMSys) refresh(objAPI ObjectLayer) error {
	iamUsersMap := make(map[string]auth.Credentials)
	iamGroupsMap := make(map[string]GroupInfo)
	iamPolicyDocsMap := make(map[string]iampolicy.Policy)
	iamUserPolicyMap := make(map[string]MappedPolicy)
	iamGroupPolicyMap := make(map[string]MappedPolicy)

	if err := loadPolicyDocs(objAPI, iamPolicyDocsMap); err != nil {
		return err
	}
	if err := loadUsers(objAPI, false, iamUsersMap); err != nil {
		return err
	}
	// load STS temp users into the same map
	if err := loadUsers(objAPI, true, iamUsersMap); err != nil {
		return err
	}
	if err := loadGroups(objAPI, iamGroupsMap); err != nil {
		return err
	}

	if err := loadMappedPolicies(objAPI, false, false, iamUserPolicyMap); err != nil {
		return err
	}
	// load STS policy mappings into the same map
	if err := loadMappedPolicies(objAPI, true, false, iamUserPolicyMap); err != nil {
		return err
	}
	// load policies mapped to groups
	if err := loadMappedPolicies(objAPI, false, false, iamGroupPolicyMap); err != nil {
		return err
	}

	// Sets default canned policies, if none are set.
	setDefaultCannedPolicies(iamPolicyDocsMap)

	sys.Lock()
	defer sys.Unlock()

	sys.iamUsersMap = iamUsersMap
	sys.iamPolicyDocsMap = iamPolicyDocsMap
	sys.iamUserPolicyMap = iamUserPolicyMap
	sys.iamGroupPolicyMap = iamGroupPolicyMap
	sys.iamGroupsMap = iamGroupsMap
	sys.buildUserGroupMemberships()

	return nil
}

// buildUserGroupMemberships - builds the memberships map. IMPORTANT:
// Assumes that sys.Lock is held by caller.
func (sys *IAMSys) buildUserGroupMemberships() {
	for group, gi := range sys.iamGroupsMap {
		sys.updateGroupMembershipsMap(group, &gi)
	}
}

// updateGroupMembershipsMap - updates the memberships map for a
// group. IMPORTANT: Assumes sys.Lock() is held by caller.
func (sys *IAMSys) updateGroupMembershipsMap(group string, gi *GroupInfo) {
	if gi == nil {
		return
	}
	for _, member := range gi.Members {
		v := sys.iamUserGroupMemberships[member]
		if v == nil {
			v = set.CreateStringSet(group)
		} else {
			v.Add(group)
		}
		sys.iamUserGroupMemberships[member] = v
	}
}

// removeGroupFromMembershipsMap - removes the group from every member
// in the cache. IMPORTANT: Assumes sys.Lock() is held by caller.
func (sys *IAMSys) removeGroupFromMembershipsMap(group string) {
	if _, ok := sys.iamUserGroupMemberships[group]; !ok {
		return
	}
	for member, groups := range sys.iamUserGroupMemberships {
		if !groups.Contains(group) {
			continue
		}
		groups.Remove(group)
		sys.iamUserGroupMemberships[member] = groups
	}
}

// NewIAMSys - creates new config system object.
func NewIAMSys() *IAMSys {
	return &IAMSys{
		iamUsersMap:             make(map[string]auth.Credentials),
		iamPolicyDocsMap:        make(map[string]iampolicy.Policy),
		iamUserPolicyMap:        make(map[string]MappedPolicy),
		iamGroupsMap:            make(map[string]GroupInfo),
		iamUserGroupMemberships: make(map[string]set.StringSet),
	}
}
