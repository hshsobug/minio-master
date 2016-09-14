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

import (
	"net/rpc"
	"time"

	"github.com/minio/dsync"
)

// AuthRPCClient is a wrapper type for RPCClient which provides JWT based authentication across reconnects.
type AuthRPCClient struct {
	rpc         *RPCClient // reconnect'able rpc client built on top of net/rpc Client
	cred        credential // AccessKey and SecretKey
	isLoggedIn  bool       // Indicates if the auth client has been logged in and token is valid.
	token       string     // JWT based token
	tstamp      time.Time  // Timestamp as received on Login RPC.
	loginMethod string     // RPC service name for authenticating using JWT
}

// newAuthClient - returns a jwt based authenticated (go) rpc client, which does automatic reconnect.
func newAuthClient(node, rpcPath string, cred credential, loginMethod string) *AuthRPCClient {
	return &AuthRPCClient{
		rpc:         newClient(node, rpcPath),
		cred:        cred,
		isLoggedIn:  false, // Not logged in yet.
		loginMethod: loginMethod,
	}
}

// Close - closes underlying rpc connection.
func (authClient *AuthRPCClient) Close() error {
	// reset token on closing a connection
	authClient.isLoggedIn = false
	return authClient.rpc.Close()
}

// Login - a jwt based authentication is performed with rpc server.
func (authClient *AuthRPCClient) Login() error {
	// Return if already logged in.
	if authClient.isLoggedIn {
		return nil
	}
	reply := RPCLoginReply{}
	if err := authClient.rpc.Call(authClient.loginMethod, RPCLoginArgs{
		Username: authClient.cred.AccessKeyID,
		Password: authClient.cred.SecretAccessKey,
	}, &reply); err != nil {
		return err
	}
	// Set token, time stamp as received from a successful login call.
	authClient.token = reply.Token
	authClient.tstamp = reply.Timestamp
	authClient.isLoggedIn = true
	return nil
}

// Call - If rpc connection isn't established yet since previous disconnect,
// connection is established, a jwt authenticated login is performed and then
// the call is performed.
func (authClient *AuthRPCClient) Call(serviceMethod string, args dsync.TokenSetter, reply interface{}) (err error) {
	// On successful login, attempt the call.
	if err = authClient.Login(); err == nil {
		// Set token and timestamp before the rpc call.
		args.SetToken(authClient.token)
		args.SetTimestamp(authClient.tstamp)

		// ..
		err = authClient.rpc.Call(serviceMethod, args, reply)
		// Invalidate token to mark for re-login on subsequent reconnect.
		if err != nil && err == rpc.ErrShutdown {
			authClient.isLoggedIn = false
		}
	}
	return err
}
